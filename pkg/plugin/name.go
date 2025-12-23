// Modified by Lorenzo Felletti (2025) under Apache 2.0.
// Original code from vcluster-generic-crd-sync-plugin by Loft Labs.

package plugin

import (
	"os"
)

// PluginNameEnv is the environment variable that contains the plugin name. Name inferred by looking at the loft-sh github organization.
const PluginNameEnv = "VCLUSTER_PLUGIN_NAME"

func GetPluginName() string {
	return os.Getenv(PluginNameEnv)
}
