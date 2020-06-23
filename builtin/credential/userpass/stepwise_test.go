package userpass

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/helper/policyutil"
	"github.com/mitchellh/mapstructure"

	"github.com/hashicorp/vault/sdk/testing/stepwise"
	dockerEnvironment "github.com/hashicorp/vault/sdk/testing/stepwise/environments/docker"
)

func TestAccBackend_stepwise_UserCrud(t *testing.T) {
	customPluginName := "my-userpass"
	envOptions := &stepwise.MountOptions{
		RegistryName:    customPluginName,
		PluginType:      stepwise.PluginTypeCredential,
		PluginName:      "userpass",
		MountPathPrefix: customPluginName,
	}
	stepwise.Run(t, stepwise.Case{
		Environment: dockerEnvironment.NewEnvironment(customPluginName, envOptions),
		Steps: []stepwise.Step{
			testAccStepwiseUser(t, "web", "password", "foo"),
			testAccStepwiseReadUser(t, "web", "foo"),
			testAccStepwiseDeleteUser(t, "web"),
			testAccStepwiseReadUser(t, "web", ""),
		},
	})
}

func testAccStepwiseUser(
	t *testing.T, name string, password string, policies string) stepwise.Step {
	return stepwise.Step{
		Operation: stepwise.UpdateOperation,
		Path:      "users/" + name,
		Data: map[string]interface{}{
			"password": password,
			"policies": policies,
		},
	}
}

func testAccStepwiseDeleteUser(t *testing.T, n string) stepwise.Step {
	return stepwise.Step{
		Operation: stepwise.DeleteOperation,
		Path:      "users/" + n,
	}
}

func testAccStepwiseReadUser(t *testing.T, name string, policies string) stepwise.Step {
	return stepwise.Step{
		Operation: stepwise.ReadOperation,
		Path:      "users/" + name,
		Assert: func(resp *api.Secret, err error) error {
			if resp == nil {
				if policies == "" {
					return nil
				}

				return fmt.Errorf("bad: %#v", resp)
			}

			var d struct {
				Policies []string `mapstructure:"policies"`
			}
			if err := mapstructure.Decode(resp.Data, &d); err != nil {
				return err
			}

			if !reflect.DeepEqual(d.Policies, policyutil.ParsePolicies(policies)) {
				return fmt.Errorf("bad: %#v", resp)
			}

			return nil
		},
	}
}
