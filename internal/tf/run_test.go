package tf

import "testing"

func TestTerraformEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"AWS_PROFILE=dev",
		"AWS_DEFAULT_PROFILE=dev",
		"AWS_ACCESS_KEY_ID=AKIA-ENV",
		"AWS_SECRET_ACCESS_KEY=secret",
		"AWS_SESSION_TOKEN=token",
		"AWS_REGION=eu-west-1",
		"TF_LOG=DEBUG", // managed by tfexec, must be dropped
	}

	t.Run("--profile wins over env profile and static keys", func(t *testing.T) {
		env := terraformEnv(environ, "prod", "/cache/plugins")
		if env["AWS_PROFILE"] != "prod" {
			t.Errorf("AWS_PROFILE = %q, want prod", env["AWS_PROFILE"])
		}
		for _, k := range []string{"AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "TF_LOG"} {
			if _, ok := env[k]; ok {
				t.Errorf("%s should be removed", k)
			}
		}
		if env["PATH"] != "/usr/bin" || env["AWS_REGION"] != "eu-west-1" {
			t.Error("unrelated variables must be kept")
		}
		if env["TF_PLUGIN_CACHE_DIR"] != "/cache/plugins" {
			t.Errorf("TF_PLUGIN_CACHE_DIR = %q", env["TF_PLUGIN_CACHE_DIR"])
		}
	})

	t.Run("no --profile inherits the environment", func(t *testing.T) {
		env := terraformEnv(environ, "", "")
		if env["AWS_PROFILE"] != "dev" || env["AWS_ACCESS_KEY_ID"] != "AKIA-ENV" {
			t.Error("without --profile the AWS environment must pass through unchanged")
		}
		if _, ok := env["TF_PLUGIN_CACHE_DIR"]; ok {
			t.Error("no plugin cache was given")
		}
	})
}
