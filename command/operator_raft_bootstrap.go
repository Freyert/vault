package command

import (
	"fmt"
	"strings"

	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

var _ cli.Command = (*OperatorRaftJoinCommand)(nil)
var _ cli.CommandAutocomplete = (*OperatorRaftJoinCommand)(nil)

type OperatorRaftJoinCommand struct {
	flagRetry            bool
	flagLeaderCACert     string
	flagLeaderClientCert string
	flagLeaderClientKey  string
	flagNonVoter         bool
	*BaseCommand
}

func (c *OperatorRaftJoinCommand) Synopsis() string {
	return "Bootstraps a node to be the initial active Raft node"
}

func (c *OperatorRaftJoinCommand) Help() string {
	helpText := `
Usage: vault operator raft bootstrap

  Bootstrap the current node as the active node for a Raft cluster.

      $ vault operator raft bootstrap

` + c.Flags().Help()

	return strings.TrimSpace(helpText)
}

func (c *OperatorRaftJoinCommand) Flags() *FlagSets {
	return c.flagSet(FlagSetHTTP)
}

func (c *OperatorRaftJoinCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *OperatorRaftJoinCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *OperatorRaftJoinCommand) Run(args []string) int {
	f := c.Flags()

	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	args = f.Args()
	if len(args) > 0 {
		c.UI.Error(fmt.Sprintf("Too many arguments (expected 0, got %d)", len(args)))
		return 1
	}

	client, err := c.Client()
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}

	if err := client.Sys().RaftBootstrap(); err != nil {
		c.UI.Error(fmt.Sprintf("Error performing Raft bootstrap the node: %s", err))
		return 2
	}

	c.UI.Output(fmt.Sprintf("Success! Performed Raft bootstrap on the node."))
	return 0
}