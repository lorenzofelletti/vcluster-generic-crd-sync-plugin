package plugin

import (
	"os"
)

// PluginNameEnv is the environment variable that contains the plugin name. Name inferred by looking at the loft-sh github organization.
const PluginNameEnv = "VCLUSTER_PLUGIN_NAME"

func GetPluginName() string {
	return os.Getenv(PluginNameEnv)
}
