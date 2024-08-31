package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/briandowns/spinner"
	"github.com/skratchdot/open-golang/open"
	"github.com/spf13/cobra"

	"github.com/iximiuz/labctl/internal/labcli"
	"github.com/iximiuz/labctl/internal/ssh"
)

const (
	loginSessionTimeout = 10 * time.Minute
)

type loginOptions struct {
	sessionID   string
	accessToken string
}

func newLoginCommand(cli labcli.CLI) *cobra.Command {
	var opts loginOptions

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in as a Labs user (you will be prompted to open a browser page with a one-time use URL)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.sessionID != "" && opts.accessToken == "" {
				return labcli.NewStatusError(1,
					"Access token must be provided if session ID is specified.",
				)
			}
			if opts.sessionID == "" && opts.accessToken != "" {
				return labcli.NewStatusError(1,
					"Session ID must be provided if access token is specified.",
				)
			}

			return labcli.WrapStatusError(runLogin(cmd.Context(), cli, opts))
		},
	}

	flags := cmd.Flags()

	flags.StringVarP(
		&opts.sessionID,
		"session-id",
		"s",
		"",
		`Session ID`,
	)
	flags.StringVarP(
		&opts.accessToken,
		"access-token",
		"t",
		"",
		`Access token`,
	)

	return cmd
}

func runLogin(ctx context.Context, cli labcli.CLI, opts loginOptions) error {
	if cli.Config().SessionID != "" && cli.Config().AccessToken != "" {
		return labcli.NewStatusError(1,
			"Already logged in. Use 'labctl auth logout' first if you want to log in as a different user.",
		)
	}

	if opts.sessionID != "" && opts.accessToken != "" {
		cli.Client().SetCredentials(opts.sessionID, opts.accessToken)
		if err := saveSessionAndGenerateSSHIdentity(cli, opts.sessionID, opts.accessToken); err != nil {
			return err
		}
		cli.PrintAux("Authenticated.\n")
		return nil
	}

	ses, err := cli.Client().CreateSession(ctx)
	if err != nil {
		return fmt.Errorf("couldn't start a session: %w", err)
	}

	accessToken := ses.AccessToken
	cli.Client().SetCredentials(ses.ID, accessToken)

	cli.PrintAux("Opening %s in your browser...\n", ses.AuthURL)

	if err := open.Run(ses.AuthURL); err != nil {
		cli.PrintAux("Couldn't open the browser. Copy the above URL into a browser manually and follow the instructions on the page.\n")
	}

	cli.PrintAux("\n")

	s := spinner.New(spinner.CharSets[39], 300*time.Millisecond)
	s.Writer = cli.AuxStream()
	s.Prefix = "Waiting for the session to be authorized... "
	s.Start()

	ctx, cancel := context.WithTimeout(ctx, loginSessionTimeout)
	defer cancel()

	for ctx.Err() == nil {
		if ses, err := cli.Client().GetSession(ctx, ses.ID); err == nil && ses.Authenticated {
			s.FinalMSG = "Waiting for the session to be authorized... Done.\n"
			s.Stop()

			if err := saveSessionAndGenerateSSHIdentity(cli, ses.ID, accessToken); err != nil {
				return err
			}

			cli.PrintAux("\nSession authorized. You can now use labctl commands.\n")
			return nil
		}

		time.Sleep(2 * time.Second)
	}

	return nil
}

func saveSessionAndGenerateSSHIdentity(cli labcli.CLI, sessionID, accessToken string) error {
	cli.Config().SessionID = sessionID
	cli.Config().AccessToken = accessToken
	if err := cli.Config().Dump(); err != nil {
		return fmt.Errorf("couldn't save the credentials to the config file: %w", err)
	}

	if err := ssh.GenerateIdentity(cli.Config().SSHDir); err != nil {
		return fmt.Errorf("couldn't generate SSH identity in %s: %w", cli.Config().SSHDir, err)
	}

	return nil
}
