package main

import (
	"os"

	"github.com/BishopFox/cloudfox/cli"
	"github.com/spf13/cobra"
)

var (
	rootCmd = &cobra.Command{
		Use:     os.Args[0],
		Version: "1.11.2",
	}
)

func main() {
	rootCmd.AddCommand(cli.AWSCommands, cli.AzCommands)
	rootCmd.Execute()
}
