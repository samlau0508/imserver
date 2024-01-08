package cmd

import "github.com/spf13/cobra"

type IMContext struct {
}

type CMD interface {
	CMD() *cobra.Command
}
