package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/accesstoken"
)

// newAccessTokenCmd is `spored access-token`: how a client opens this
// instance's DCV session or web proxy without the token ever being published.
// The client mints the token and runs `access-token add` over SSM with only its
// SHA-256; spored checks a presented token against the registered hashes
// (pkg/accesstoken). The subcommand name and flags are a wire contract with
// spawn (cmd/accesstoken.go), spawn-ts and the portal.
func newAccessTokenCmd() *cobra.Command {
	return newAccessTokenCmdWith(accesstoken.NewStore())
}

func newAccessTokenCmdWith(store *accesstoken.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access-token",
		Short: "Register or revoke the tokens that open this instance's DCV session or web app",
	}

	var kind, hash string
	var ttl time.Duration
	add := &cobra.Command{
		Use:   "add",
		Short: "Register a token by its SHA-256 (the token itself never reaches the instance)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := accesstoken.ParseKind(kind)
			if err != nil {
				return err
			}
			exp, err := store.Add(k, hash, ttl)
			if err != nil {
				return err
			}
			// Prints the expiry only: nothing here is secret, and SSM keeps it.
			fmt.Fprintf(cmd.OutOrStdout(), "access-token registered: kind=%s expires=%s\n", k, exp.Format(time.RFC3339))
			return nil
		},
	}
	add.Flags().StringVar(&kind, "kind", "", "what the token opens: dcv or web")
	add.Flags().StringVar(&hash, "sha256", "", "hex SHA-256 of the token")
	add.Flags().DurationVar(&ttl, "ttl", accesstoken.DefaultTTL, "how long the token stays valid (at most 168h)")
	_ = add.MarkFlagRequired("kind")
	_ = add.MarkFlagRequired("sha256")

	revoke := &cobra.Command{
		Use:   "revoke-all",
		Short: "Revoke every registered token (open sessions keep running; new connections need a new token)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := store.RevokeAll(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "access-tokens revoked")
			return nil
		},
	}

	count := &cobra.Command{
		Use:   "count",
		Short: "Print how many unexpired tokens are registered",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := store.Prune()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d\n", n)
			return nil
		},
	}

	cmd.AddCommand(add, revoke, count)
	return cmd
}
