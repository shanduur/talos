// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package generate

import (
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
)

func (in *Input) generateKubernetesControlplaneConfigs() []config.Document {
	if !in.Options.VersionContract.MultidocKubernetesConfigSupported() {
		return nil
	}

	etcdEncryptionConfig := k8s.NewKubeEtcdEncryptionConfigV1Alpha1()
	etcdEncryptionConfig.Config.Object = map[string]any{
		"resources": []any{
			map[string]any{
				"providers": []any{
					map[string]any{
						"secretbox": map[string]any{
							"keys": []any{
								map[string]any{
									"name":   "key1",
									"secret": in.Options.SecretsBundle.Secrets.SecretboxEncryptionSecret,
								},
							},
						},
					},
				},
				"resources": []any{
					"secrets",
				},
			},
		},
	}

	return []config.Document{
		etcdEncryptionConfig,
	}
}
