package sealmigration

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-test/deep"

	"github.com/hashicorp/go-hclog"
	wrapping "github.com/hashicorp/go-kms-wrapping"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/helper/testhelpers"
	sealhelper "github.com/hashicorp/vault/helper/testhelpers/seal"
	"github.com/hashicorp/vault/helper/testhelpers/teststorage"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/physical/raft"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/vault"
)

const (
	numTestCores = 5
	keyShares    = 3
	keyThreshold = 3

	basePort_ShamirToTransit_Pre14  = 20000
	basePort_TransitToShamir_Pre14  = 21000
	basePort_ShamirToTransit_Post14 = 22000
	basePort_TransitToShamir_Post14 = 23000
	basePort_TransitToTransit       = 24000
	basePort_TransitToTestSeal      = 25000
)

type testFunc func(t *testing.T, logger hclog.Logger, storage teststorage.ReusableStorage, basePort int)

func testVariousBackends(t *testing.T, tf testFunc, basePort int, includeRaft bool) {

	logger := logging.NewVaultLogger(hclog.Debug).Named(t.Name())

	t.Run("inmem", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("inmem")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeInmemBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, basePort+100)
	})

	t.Run("file", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("file")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeFileBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, basePort+200)
	})

	t.Run("consul", func(t *testing.T) {
		t.Parallel()

		logger := logger.Named("consul")
		storage, cleanup := teststorage.MakeReusableStorage(
			t, logger, teststorage.MakeConsulBackend(t, logger))
		defer cleanup()
		tf(t, logger, storage, basePort+300)
	})

	if includeRaft {
		t.Run("raft", func(t *testing.T) {
			t.Parallel()

			logger := logger.Named("raft")
			raftBasePort := basePort + 400

			atomic.StoreUint32(&vault.UpdateClusterAddrForTests, 1)
			addressProvider := testhelpers.NewHardcodedServerAddressProvider(numTestCores, raftBasePort+10)

			storage, cleanup := teststorage.MakeReusableRaftStorage(t, logger, numTestCores, addressProvider)
			defer cleanup()
			tf(t, logger, storage, raftBasePort)
		})
	}
}

// TestSealMigration_ShamirToTransit_Pre14 tests shamir-to-transit seal
// migration, using the pre-1.4 method of bring down the whole cluster to do
// the migration.
func TestSealMigration_ShamirToTransit_Pre14(t *testing.T) {
	// Note that we do not test integrated raft storage since this is
	// a pre-1.4 test.
	testVariousBackends(t, testSealMigrationShamirToTransit_Pre14, basePort_ShamirToTransit_Pre14, false)
}

func testSealMigrationShamirToTransit_Pre14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Initialize the backend using shamir
	cluster, _ := initializeShamir(t, logger, storage, basePort)
	rootToken, barrierKeys := cluster.RootToken, cluster.BarrierKeys
	cluster.EnsureCoresSealed(t)
	storage.Cleanup(t, cluster)
	cluster.Cleanup()

	// Create the transit server.
	tss := sealhelper.NewTransitSealServer(t)
	defer func() {
		tss.EnsureCoresSealed(t)
		tss.Cleanup()
	}()
	tss.MakeKey(t, "transit-seal-key-1")

	// Migrate the backend from shamir to transit.  Note that the barrier keys
	// are now the recovery keys.
	transitSeal := migrateFromShamirToTransit_Pre14(t, logger, storage, basePort, tss, rootToken, barrierKeys)

	// Run the backend with transit.
	runAutoseal(t, logger, storage, basePort, rootToken, transitSeal, 0)
}

func migrateFromShamirToTransit_Pre14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer, rootToken string, recoveryKeys [][]byte,
) vault.Seal {

	var baseClusterPort = basePort + 10

	var transitSeal vault.Seal

	var conf = vault.CoreConfig{
		Logger:                    logger.Named("migrateFromShamirToTransit"),
		DisablePerformanceStandby: true,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
		// N.B. Providing a transit seal puts us in migration mode.
		SealFunc: func() vault.Seal {
			transitSeal = tss.MakeSeal(t, "transit-seal-key-1")
			return transitSeal
		},
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	leader.Client.SetToken(rootToken)

	// Unseal and migrate to Transit.
	unsealMigrate(t, leader.Client, recoveryKeys, true)

	// Wait for migration to finish.
	awaitMigration(t, leader.Client)

	// Read the secret
	secret, err := leader.Client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Make sure the seal configs were updated correctly.
	b, r, err := leader.Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)

	cluster.EnsureCoresSealed(t)

	return transitSeal
}

// TestSealMigration_ShamirToTransit_Post14 tests shamir-to-transit seal
// migration, using the post-1.4 method of bring individual nodes in the cluster
// to do the migration.
func TestSealMigration_ShamirToTransit_Post14(t *testing.T) {
	testVariousBackends(t, testSealMigrationShamirToTransit_Post14, basePort_ShamirToTransit_Post14, true)
}

func testSealMigrationShamirToTransit_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Initialize the backend using shamir
	cluster, opts := initializeShamir(t, logger, storage, basePort)

	// Create the transit server.
	tss := sealhelper.NewTransitSealServer(t)
	defer func() {
		tss.EnsureCoresSealed(t)
		tss.Cleanup()
	}()
	tss.MakeKey(t, "transit-seal-key-1")

	// Migrate the backend from shamir to transit.
	transitSeal := migrateFromShamirToTransit_Post14(t, logger, storage, basePort, tss, cluster, opts)
	cluster.EnsureCoresSealed(t)

	storage.Cleanup(t, cluster)
	cluster.Cleanup()

	// Run the backend with transit.
	runAutoseal(t, logger, storage, basePort, cluster.RootToken, transitSeal, 0)
}

func migrateFromShamirToTransit_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer,
	cluster *vault.TestCluster, opts *vault.TestClusterOptions,
) vault.Seal {

	// N.B. Providing a transit seal puts us in migration mode.
	var transitSeal vault.Seal
	opts.SealFunc = func() vault.Seal {
		transitSeal = tss.MakeSeal(t, "transit-seal-key-1")
		return transitSeal
	}
	modifyCoreConfig := func(tcc *vault.TestClusterCore) {}

	// Restart each follower with the new config, and migrate to Transit.
	// Note that the barrier keys are being used as recovery keys.
	leaderIdx := migratePost14(
		t, logger, storage, cluster, opts,
		cluster.RootToken, cluster.BarrierKeys,
		migrateShamirToTransit, modifyCoreConfig)
	leader := cluster.Cores[leaderIdx]

	// Read the secret
	secret, err := leader.Client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Make sure the seal configs were updated correctly.
	b, r, err := leader.Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)

	return transitSeal
}

// TestSealMigration_TransitToShamir_Post14 tests transit-to-shamir seal
// migration, using the post-1.4 method of bring individual nodes in the
// cluster to do the migration.
func TestSealMigration_TransitToShamir_Post14(t *testing.T) {
	testVariousBackends(t, testSealMigrationTransitToShamir_Post14, basePort_TransitToShamir_Post14, true)
}

func testSealMigrationTransitToShamir_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) {

	// Create the transit server.
	tss := sealhelper.NewTransitSealServer(t)
	defer func() {
		if tss != nil {
			tss.Cleanup()
		}
	}()
	tss.MakeKey(t, "transit-seal-key-1")

	// Initialize the backend with transit.
	cluster, opts, transitSeal := initializeTransit(t, logger, storage, basePort, tss)
	rootToken, recoveryKeys := cluster.RootToken, cluster.RecoveryKeys

	// Migrate the backend from transit to shamir
	migrateFromTransitToShamir_Post14(t, logger, storage, basePort, transitSeal, cluster, opts)
	cluster.EnsureCoresSealed(t)
	storage.Cleanup(t, cluster)
	cluster.Cleanup()

	// Now that migration is done, we can nuke the transit server, since we
	// can unseal without it.
	tss.Cleanup()
	tss = nil

	// Run the backend with shamir.  Note that the recovery keys are now the
	// barrier keys.
	runShamir(t, logger, storage, basePort, rootToken, recoveryKeys)
}

func migrateFromTransitToShamir_Post14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	transitSeal vault.Seal,
	cluster *vault.TestCluster, opts *vault.TestClusterOptions) {

	opts.SealFunc = nil
	modifyCoreConfig := func(tcc *vault.TestClusterCore) {
		// Nil out the seal so it will be initialized as shamir.
		tcc.CoreConfig.Seal = nil

		// N.B. Providing an UnwrapSeal puts us in migration mode. This is the
		// equivalent of doing the following in HCL:
		//     seal "transit" {
		//       // ...
		//       disabled = "true"
		//     }
		tcc.CoreConfig.UnwrapSeal = transitSeal
	}

	// Restart each follower with the new config, and migrate to Shamir.
	leaderIdx := migratePost14(
		t, logger, storage, cluster, opts,
		cluster.RootToken, cluster.RecoveryKeys,
		migrateTransitToShamir, modifyCoreConfig)
	leader := cluster.Cores[leaderIdx]

	// Read the secret
	secret, err := leader.Client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Make sure the seal configs were updated correctly.
	b, r, err := cluster.Cores[0].Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Shamir, keyShares, keyThreshold, 1)
	if r != nil {
		t.Fatalf("expected nil recovery config, got: %#v", r)
	}
}

//// TestSealMigration_TransitToTransit tests transit-to-shamir seal
//// migration, using the post-1.4 method of bring individual nodes in the
//// cluster to do the migration.
//func TestSealMigration_TransitToTransit(t *testing.T) {
//	testVariousBackends(t, testSealMigration_TransitToTransit, basePort_TransitToTransit, true)
//}
//
//func testSealMigration_TransitToTransit(
//	t *testing.T, logger hclog.Logger,
//	storage teststorage.ReusableStorage, basePort int) {
//
//	// Create the transit server.
//	tss1 := sealhelper.NewTransitSealServer(t)
//	defer func() {
//		if tss1 != nil {
//			tss1.Cleanup()
//		}
//	}()
//	tss1.MakeKey(t, "transit-seal-key-1")
//
//	// Initialize the backend with transit.
//	cluster, opts, transitSeal1 := initializeTransit(t, logger, storage, basePort, tss1)
//	rootToken := cluster.RootToken
//
//	// Create the transit server.
//	tss2 := sealhelper.NewTransitSealServer(t)
//	defer func() {
//		tss2.EnsureCoresSealed(t)
//		tss2.Cleanup()
//	}()
//	tss2.MakeKey(t, "transit-seal-key-2")
//
//	// Migrate the backend from transit to transit.
//	transitSeal2, leaderIdx := migrateFromTransitToTransit(t, logger, storage, basePort, transitSeal1, tss2, cluster, opts)
//
//	// Now that migration is done, we can nuke the transit server, since we
//	// can unseal without it.
//	tss1.EnsureCoresSealed(t)
//	tss1.Cleanup()
//	tss1 = nil
//
//	// Run the backend with transit.
//	runAutoseal(t, logger, storage, basePort+50, rootToken, transitSeal2, leaderIdx)
//}
//
//func migrateFromTransitToTransit(
//	t *testing.T, logger hclog.Logger,
//	storage teststorage.ReusableStorage, basePort int,
//	transitSeal1 vault.Seal,
//	tss2 *sealhelper.TransitSealServer,
//	cluster *vault.TestCluster, opts *vault.TestClusterOptions,
//) (vault.Seal, int) {
//
//	// N.B. Providing a transit seal puts us in migration mode.
//	var transitSeal2 vault.Seal
//	opts.SealFunc = func() vault.Seal {
//		transitSeal2 = tss2.MakeSeal(t, "transit-seal-key-1")
//		return transitSeal2
//	}
//
//	modifyCoreConfig := func(tcc *vault.TestClusterCore) {
//		// Nil out the seal so it will be initialized with the SealFunc.
//		tcc.CoreConfig.Seal = nil
//
//		// N.B. Providing an UnwrapSeal puts us in migration mode. This is the
//		// equivalent of doing the following in HCL:
//		//     seal "transit" {
//		//       // ...
//		//       disabled = "true"
//		//     }
//		tcc.CoreConfig.UnwrapSeal = transitSeal1
//	}
//
//	// Restart each follower with the new config, and migrate to transit.
//	leaderIdx := migratePost14(
//		t, logger, storage, cluster, opts,
//		cluster.RootToken, cluster.RecoveryKeys,
//		migrateTransitToTransit, modifyCoreConfig)
//	leader := cluster.Cores[leaderIdx]
//
//	// Read the secret
//	secret, err := leader.Client.Logical().Read("secret/foo")
//	if err != nil {
//		t.Fatal(err)
//	}
//	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
//		t.Fatal(diff)
//	}
//
//	// Make sure the seal configs were updated correctly.
//	b, r, err := cluster.Cores[0].Core.PhysicalSealConfigs(context.Background())
//	if err != nil {
//		t.Fatal(err)
//	}
//	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
//	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)
//
//	return transitSeal2, leaderIdx
//}

//// TestSealMigration_TransitToTestSeal tests transit-to-shamir seal
//// migration, using the post-1.4 method of bring individual nodes in the
//// cluster to do the migration.
//func TestSealMigration_TransitToTestSeal(t *testing.T) {
//	testVariousBackends(t, testSealMigration_TransitToTestSeal, basePort_TransitToTestSeal, true)
//}
//
//func testSealMigration_TransitToTestSeal(
//	t *testing.T, logger hclog.Logger,
//	storage teststorage.ReusableStorage, basePort int) {
//
//	// Create the transit server.
//	tss1 := sealhelper.NewTransitSealServer(t)
//	defer func() {
//		if tss1 != nil {
//			tss1.Cleanup()
//		}
//	}()
//	tss1.MakeKey(t, "transit-seal-key-1")
//
//	// Initialize the backend with transit.
//	cluster, opts, transitSeal1 := initializeTransit(t, logger, storage, basePort, tss1)
//	rootToken := cluster.RootToken
//
//	// Migrate the backend from transit to transit.
//	testSeal := vault.NewAutoSeal(vaultseal.NewTestSeal(&vaultseal.TestSealOpts{}))
//	leaderIdx := migrateFromTransitToTestSeal(t, logger, storage, basePort, transitSeal1, testSeal, cluster, opts)
//
//	// Now that migration is done, we can nuke the transit server, since we
//	// can unseal without it.
//	tss1.EnsureCoresSealed(t)
//	tss1.Cleanup()
//	tss1 = nil
//
//	// Run the backend with transit.
//	runAutoseal(t, logger, storage, basePort+50, rootToken, testSeal, leaderIdx)
//}
//
//func migrateFromTransitToTestSeal(
//	t *testing.T, logger hclog.Logger,
//	storage teststorage.ReusableStorage, basePort int,
//	transitSeal1 vault.Seal, testSeal vault.Seal,
//	cluster *vault.TestCluster, opts *vault.TestClusterOptions,
//) int {
//
//	modifyCoreConfig := func(tcc *vault.TestClusterCore) {
//		tcc.CoreConfig.Seal = testSeal
//
//		// N.B. Providing an UnwrapSeal puts us in migration mode. This is the
//		// equivalent of doing the following in HCL:
//		//     seal "transit" {
//		//       // ...
//		//       disabled = "true"
//		//     }
//		tcc.CoreConfig.UnwrapSeal = transitSeal1
//	}
//
//	// Restart each follower with the new config, and migrate to transit.
//	leaderIdx := migratePost14(
//		t, logger, storage, cluster, opts,
//		cluster.RootToken, cluster.RecoveryKeys,
//		migrateTransitToTestSeal, modifyCoreConfig)
//	leader := cluster.Cores[leaderIdx]
//
//	// Read the secret
//	secret, err := leader.Client.Logical().Read("secret/foo")
//	if err != nil {
//		t.Fatal(err)
//	}
//	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
//		t.Fatal(diff)
//	}
//
//	// Make sure the seal configs were updated correctly.
//	b, r, err := cluster.Cores[0].Core.PhysicalSealConfigs(context.Background())
//	if err != nil {
//		t.Fatal(err)
//	}
//	verifyBarrierConfig(t, b, wrapping.Test, 1, 1, 1)
//	verifyBarrierConfig(t, r, wrapping.Shamir, keyShares, keyThreshold, 0)
//
//	return leaderIdx
//}

type migrationDirection int

const (
	migrateShamirToTransit migrationDirection = iota
	migrateTransitToShamir
	migrateTransitToTransit
	migrateTransitToTestSeal
)

func migratePost14(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage,
	cluster *vault.TestCluster, opts *vault.TestClusterOptions,
	rootToken string, recoveryKeys [][]byte,
	migrate migrationDirection,
	modifyCoreConfig func(*vault.TestClusterCore),
) int {

	// Restart each follower with the new config, and migrate.
	for i := 1; i < len(cluster.Cores); i++ {
		cluster.StopCore(t, i)
		if storage.IsRaft {
			teststorage.CloseRaftStorage(t, cluster, i)
		}
		modifyCoreConfig(cluster.Cores[i])
		cluster.StartCore(t, i, opts)

		cluster.Cores[i].Client.SetToken(rootToken)
		unsealMigrate(t, cluster.Cores[i].Client, recoveryKeys, true)
		time.Sleep(5 * time.Second)
	}

	// Bring down the leader
	cluster.StopCore(t, 0)
	if storage.IsRaft {
		teststorage.CloseRaftStorage(t, cluster, 0)
	}

	// Wait for the followers to establish a new leader
	leaderIdx, err := testhelpers.AwaitLeader(t, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if leaderIdx == 0 {
		t.Fatalf("Core 0 cannot be the leader right now")
	}
	leader := cluster.Cores[leaderIdx]
	leader.Client.SetToken(rootToken)

	// Bring core 0 back up
	cluster.StartCore(t, 0, opts)
	cluster.Cores[0].Client.SetToken(rootToken)

	// TODO look into why this is different for different migration directions,
	// and why it is swapped for raft.
	switch migrate {
	case migrateShamirToTransit:
		if storage.IsRaft {
			unsealMigrate(t, cluster.Cores[0].Client, recoveryKeys, true)
		} else {
			unseal(t, cluster.Cores[0].Client, recoveryKeys)
		}
	case migrateTransitToShamir:
		if storage.IsRaft {
			unseal(t, cluster.Cores[0].Client, recoveryKeys)
		} else {
			unsealMigrate(t, cluster.Cores[0].Client, recoveryKeys, true)
		}
	case migrateTransitToTransit:
		if storage.IsRaft {
			panic("TODO unsealing doesn't work for raft")
			//unseal(t, cluster.Cores[0].Client, recoveryKeys)
		} else {
			unseal(t, cluster.Cores[0].Client, recoveryKeys)
		}
	case migrateTransitToTestSeal:
		if storage.IsRaft {
			panic("TODO unsealing doesn't work for raft")
			//unseal(t, cluster.Cores[0].Client, recoveryKeys)
		} else {
			unseal(t, cluster.Cores[0].Client, recoveryKeys)
		}
	default:
		t.Fatalf("unreachable")
	}

	// Wait for migration to finish.
	awaitMigration(t, leader.Client)

	// This is apparently necessary for the raft cluster to get itself
	// situated.
	if storage.IsRaft {
		time.Sleep(15 * time.Second)
		if err := testhelpers.VerifyRaftConfiguration(leader, len(cluster.Cores)); err != nil {
			t.Fatal(err)
		}
	}

	return leaderIdx
}

func unsealMigrate(t *testing.T, client *api.Client, keys [][]byte, transitServerAvailable bool) {

	for i, key := range keys {

		// Try to unseal with missing "migrate" parameter
		_, err := client.Sys().UnsealWithOptions(&api.UnsealOpts{
			Key: base64.StdEncoding.EncodeToString(key),
		})
		if err == nil {
			t.Fatal("expected error due to lack of migrate parameter")
		}

		// Unseal with "migrate" parameter
		resp, err := client.Sys().UnsealWithOptions(&api.UnsealOpts{
			Key:     base64.StdEncoding.EncodeToString(key),
			Migrate: true,
		})

		if i < keyThreshold-1 {
			// Not enough keys have been provided yet.
			if err != nil {
				t.Fatal(err)
			}
		} else {
			if transitServerAvailable {
				// The transit server is running.
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil || resp.Sealed {
					t.Fatalf("expected unsealed state; got %#v", resp)
				}
			} else {
				// The transit server is stopped.
				if err == nil {
					t.Fatal("expected error due to transit server being stopped.")
				}
			}
			break
		}
	}
}

// awaitMigration waits for migration to finish.
func awaitMigration(t *testing.T, client *api.Client) {

	timeout := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(timeout) {
			break
		}

		resp, err := client.Sys().SealStatus()
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Migration {
			return
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("migration did not complete.")
}

func unseal(t *testing.T, client *api.Client, keys [][]byte) {

	for i, key := range keys {

		resp, err := client.Sys().UnsealWithOptions(&api.UnsealOpts{
			Key: base64.StdEncoding.EncodeToString(key),
		})
		if i < keyThreshold-1 {
			// Not enough keys have been provided yet.
			if err != nil {
				t.Fatal(err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || resp.Sealed {
				t.Fatalf("expected unsealed state; got %#v", resp)
			}
			break
		}
	}
}

// verifyBarrierConfig verifies that a barrier configuration is correct.
func verifyBarrierConfig(t *testing.T, cfg *vault.SealConfig, sealType string, shares, threshold, stored int) {
	t.Helper()
	if cfg.Type != sealType {
		debug.PrintStack()
		t.Fatalf("bad seal config: %#v, expected type=%q", cfg, sealType)
	}
	if cfg.SecretShares != shares {
		t.Fatalf("bad seal config: %#v, expected SecretShares=%d", cfg, shares)
	}
	if cfg.SecretThreshold != threshold {
		t.Fatalf("bad seal config: %#v, expected SecretThreshold=%d", cfg, threshold)
	}
	if cfg.StoredShares != stored {
		t.Fatalf("bad seal config: %#v, expected StoredShares=%d", cfg, stored)
	}
}

// initializeShamir initializes a brand new backend storage with Shamir.
func initializeShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int) (*vault.TestCluster, *vault.TestClusterOptions) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger:                    logger.Named("initializeShamir"),
		DisablePerformanceStandby: true,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()

	leader := cluster.Cores[0]
	client := leader.Client

	// Unseal
	if storage.IsRaft {
		joinRaftFollowers(t, cluster, false)
		if err := testhelpers.VerifyRaftConfiguration(leader, len(cluster.Cores)); err != nil {
			t.Fatal(err)
		}
	} else {
		cluster.UnsealCores(t)
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, len(cluster.Cores))

	// Write a secret that we will read back out later.
	_, err := client.Logical().Write(
		"secret/foo",
		map[string]interface{}{"zork": "quux"})
	if err != nil {
		t.Fatal(err)
	}

	return cluster, &opts
}

// runShamir uses a pre-populated backend storage with Shamir.
func runShamir(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	rootToken string, barrierKeys [][]byte) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger:                    logger.Named("runShamir"),
		DisablePerformanceStandby: true,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[0]
	client := leader.Client
	client.SetToken(rootToken)

	// Unseal
	cluster.BarrierKeys = barrierKeys
	if storage.IsRaft {
		for _, core := range cluster.Cores {
			cluster.UnsealCore(t, core)
		}
		// This is apparently necessary for the raft cluster to get itself
		// situated.
		time.Sleep(15 * time.Second)
		if err := testhelpers.VerifyRaftConfiguration(leader, len(cluster.Cores)); err != nil {
			t.Fatal(err)
		}
	} else {
		cluster.UnsealCores(t)
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, len(cluster.Cores))

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)
}

// initializeTransit initializes a brand new backend storage with Transit.
func initializeTransit(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	tss *sealhelper.TransitSealServer) (*vault.TestCluster, *vault.TestClusterOptions, vault.Seal) {

	var transitSeal vault.Seal

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger:                    logger.Named("initializeTransit"),
		DisablePerformanceStandby: true,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SealFunc: func() vault.Seal {
			transitSeal = tss.MakeSeal(t, "transit-seal-key-1")
			return transitSeal
		},
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()

	leader := cluster.Cores[0]
	client := leader.Client

	// Join raft
	if storage.IsRaft {
		joinRaftFollowers(t, cluster, true)

		if err := testhelpers.VerifyRaftConfiguration(leader, len(cluster.Cores)); err != nil {
			t.Fatal(err)
		}
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, len(cluster.Cores))

	// Write a secret that we will read back out later.
	_, err := client.Logical().Write(
		"secret/foo",
		map[string]interface{}{"zork": "quux"})
	if err != nil {
		t.Fatal(err)
	}

	return cluster, &opts, transitSeal
}

func runAutoseal(
	t *testing.T, logger hclog.Logger,
	storage teststorage.ReusableStorage, basePort int,
	rootToken string, autoSeal vault.Seal,
	leaderIdx int) {

	var baseClusterPort = basePort + 10

	// Start the cluster
	var conf = vault.CoreConfig{
		Logger:                    logger.Named("runAutoseal"),
		DisablePerformanceStandby: true,
		Seal:                      autoSeal,
	}
	var opts = vault.TestClusterOptions{
		HandlerFunc:           vaulthttp.Handler,
		NumCores:              numTestCores,
		BaseListenAddress:     fmt.Sprintf("127.0.0.1:%d", basePort),
		BaseClusterListenPort: baseClusterPort,
		SkipInit:              true,
	}
	storage.Setup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)
	cluster.Start()
	defer func() {
		storage.Cleanup(t, cluster)
		cluster.Cleanup()
	}()

	leader := cluster.Cores[leaderIdx]
	client := leader.Client
	client.SetToken(rootToken)

	// Unseal.  Even though we are using autounseal, we have to unseal
	// explicitly because we are using SkipInit.
	for _, core := range cluster.Cores {
		cluster.UnsealCoreWithStoredKeys(t, core)
	}
	if storage.IsRaft {
		// This is apparently necessary for the raft cluster to get itself
		// situated.
		time.Sleep(15 * time.Second)
		if err := testhelpers.VerifyRaftConfiguration(leader, len(cluster.Cores)); err != nil {
			t.Fatal(err)
		}
	}
	testhelpers.WaitForNCoresUnsealed(t, cluster, len(cluster.Cores))

	// Read the secret
	secret, err := client.Logical().Read("secret/foo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := deep.Equal(secret.Data, map[string]interface{}{"zork": "quux"}); len(diff) > 0 {
		t.Fatal(diff)
	}

	// Seal the cluster
	cluster.EnsureCoresSealed(t)
}

// joinRaftFollowers unseals the leader, and then joins-and-unseals the
// followers one at a time.  We assume that the ServerAddressProvider has
// already been installed on all the nodes.
func joinRaftFollowers(t *testing.T, cluster *vault.TestCluster, useStoredKeys bool) {

	leader := cluster.Cores[0]

	cluster.UnsealCore(t, leader)
	vault.TestWaitActive(t, leader.Core)

	leaderInfos := []*raft.LeaderJoinInfo{
		&raft.LeaderJoinInfo{
			LeaderAPIAddr: leader.Client.Address(),
			TLSConfig:     leader.TLSConfig,
		},
	}

	// Join followers
	for i := 1; i < len(cluster.Cores); i++ {
		core := cluster.Cores[i]
		_, err := core.JoinRaftCluster(namespace.RootContext(context.Background()), leaderInfos, false)
		if err != nil {
			t.Fatal(err)
		}

		if useStoredKeys {
			// For autounseal, the raft backend is not initialized right away
			// after the join.  We need to wait briefly before we can unseal.
			awaitUnsealWithStoredKeys(t, core)
		} else {
			cluster.UnsealCore(t, core)
		}
	}

	testhelpers.WaitForNCoresUnsealed(t, cluster, len(cluster.Cores))
}

func awaitUnsealWithStoredKeys(t *testing.T, core *vault.TestClusterCore) {

	timeout := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(timeout) {
			t.Fatal("raft join: timeout waiting for core to unseal")
		}
		// Its actually ok for an error to happen here the first couple of
		// times -- it means the raft join hasn't gotten around to initializing
		// the backend yet.
		err := core.UnsealWithStoredKeys(context.Background())
		if err == nil {
			return
		}
		core.Logger().Warn("raft join: failed to unseal core", "error", err)
		time.Sleep(time.Second)
	}
}
