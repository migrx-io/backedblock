package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) secretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the node secrets stored in SSM Parameter Store",
		Long: `Node secrets (Cassandra, API keys, admin and Grafana passwords) live in one
SSM SecureString at secrets_ssm_path. Nodes fetch it while provisioning; it
never enters Terraform state or a local file.`,
	}
	cmd.AddCommand(a.secretsGenerateCmd(), a.secretsSetCmd(), a.secretsGetCmd(), a.secretsListCmd())
	return cmd
}

func (a *app) secretsGenerateCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Create the node secret with random values",
		Long: `generate writes random values for every key. If the secret already exists it
is left alone (exit 0, status "exists") unless --force is given. Rotating with
--force on a running cluster does not update nodes already provisioned.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := awsx.New(cmd.Context(), cfg.Region, a.profile)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			_, found, err := c.GetSecrets(cmd.Context(), cfg.SecretsSSMPath)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			status := "created"
			if found && !force {
				status = "exists"
			} else {
				s, err := awsx.GenerateSecrets()
				if err != nil {
					return err
				}
				if err := c.PutSecrets(cmd.Context(), cfg.SecretsSSMPath, s); err != nil {
					return ui.Wrap(ui.ExitAWS, "", err)
				}
				if found {
					status = "replaced"
				}
			}
			res := map[string]string{"path": cfg.SecretsSSMPath, "status": status}
			return ui.Result(res, func(w io.Writer) {
				fmt.Fprintf(w, "%s: %s\n", cfg.SecretsSSMPath, status)
				if status == "exists" {
					fmt.Fprintln(w, "(left unchanged; pass --force to regenerate)")
				}
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing secret")
	return cmd
}

func (a *app) secretsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "set KEY=VALUE...",
		Short:   "Set individual keys, keeping the others",
		Example: "  bbctl secrets set prod MGX_GW_ADMIN_PASSWD=s3cret",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := awsx.New(cmd.Context(), cfg.Region, a.profile)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			s, _, err := c.GetSecrets(cmd.Context(), cfg.SecretsSSMPath)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			if s == nil {
				s = map[string]string{}
			}
			var keys []string
			for _, kv := range args {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || !validKey(k) {
					return ui.Errf(ui.ExitUsage, "keys: "+strings.Join(awsx.SecretKeys, ", "), "bad argument %q", kv)
				}
				s[k] = v
				keys = append(keys, k)
			}
			if err := c.PutSecrets(cmd.Context(), cfg.SecretsSSMPath, s); err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			res := map[string]any{"path": cfg.SecretsSSMPath, "set": keys, "missing": awsx.MissingSecrets(s)}
			return ui.Result(res, func(w io.Writer) { fmt.Fprintf(w, "%s: set %s\n", cfg.SecretsSSMPath, strings.Join(keys, ", ")) })
		},
	}
}

func (a *app) secretsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get KEY",
		Short:   "Print one secret value (e.g. the admin password for mgx-cli)",
		Example: "  bbctl secrets get prod MGX_GW_ADMIN_PASSWD",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := awsx.New(cmd.Context(), cfg.Region, a.profile)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			s, found, err := c.GetSecrets(cmd.Context(), cfg.SecretsSSMPath)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			if !found {
				return ui.Errf(ui.ExitAWS, "run `bbctl secrets generate "+cfg.Name+"`", "node secret %s does not exist", cfg.SecretsSSMPath)
			}
			v, ok := s[args[0]]
			if !ok {
				return ui.Errf(ui.ExitUsage, "run `bbctl secrets list "+cfg.Name+"` to see the keys", "no key %q in %s", args[0], cfg.SecretsSSMPath)
			}
			return ui.Result(map[string]string{"key": args[0], "value": v}, func(w io.Writer) { fmt.Fprintln(w, v) })
		},
	}
}

func (a *app) secretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List keys in the secret (values are not printed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := awsx.New(cmd.Context(), cfg.Region, a.profile)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			s, found, err := c.GetSecrets(cmd.Context(), cfg.SecretsSSMPath)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			keys := []string{}
			for k := range s {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			res := map[string]any{"path": cfg.SecretsSSMPath, "exists": found, "keys": keys, "missing": awsx.MissingSecrets(s)}
			return ui.Result(res, func(w io.Writer) {
				if !found {
					fmt.Fprintf(w, "%s does not exist; run `bbctl secrets generate %s`\n", cfg.SecretsSSMPath, cfg.Name)
					return
				}
				fmt.Fprintf(w, "%s\n", cfg.SecretsSSMPath)
				for _, k := range keys {
					fmt.Fprintf(w, "  %s\n", k)
				}
				if m := awsx.MissingSecrets(s); len(m) > 0 {
					fmt.Fprintf(w, "missing: %s\n", strings.Join(m, ", "))
				}
			})
		},
	}
}

func validKey(k string) bool {
	for _, s := range awsx.SecretKeys {
		if s == k {
			return true
		}
	}
	return false
}
