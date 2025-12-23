package main

import (
	"os"

	"gopkg.in/yaml.v3"

	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/config"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/namecache"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/syncer"
	"github.com/loft-sh/vcluster-sdk/plugin"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog"
)

const (
	ConfigurationEnvVar    = "CONFIG"
	NewConfigurationEnvVar = "PLUGIN_CONFIG"
)

func main() {
	// init plugin
	registerCtx := plugin.MustInit()

	c := os.Getenv(NewConfigurationEnvVar)
	if c == "" {
		klog.Warningf("The %s environment variable is empty, no configuration has been loaded", NewConfigurationEnvVar)
		klog.Warningf("Falling back to the deprecated %s environment variable", ConfigurationEnvVar)
		c = os.Getenv(ConfigurationEnvVar)
	}

	if c == "" {
		// still empty
		klog.Warningf("The %s environment variable is also empty, no configuration has been loaded", ConfigurationEnvVar)
	} else {
		klog.Infof("Loading configuration:\n%s", c) //dev
		configuration, err := config.ParseConfig(c)
		if err != nil {
			klog.Fatal(err)
		}
		loadedConfig, err := yaml.Marshal(configuration)
		if err != nil {
			klog.Fatal(err)
		}
		klog.Infof("Loaded configuration:\n%s", string(loadedConfig)) //dev

		// TODO: efficiently sync all mapped CRDs from the host to vcluster or perhaps this should be a separate controller that will watch CRDs and sync changes
		// NOTE(lorenzo): if clusters are short-lived that should not be a problem and we can sync all CRDs at the start only
		for _, m := range configuration.Mappings {
			if m.FromVirtualCluster != nil {
				// if !plugin.Scheme.Recognizes(schema.FromAPIVersionAndKind(m.FromVirtualCluster.APIVersion, m.FromVirtualCluster.Kind)) {
				_, _, err := translate.EnsureCRDFromPhysicalCluster(registerCtx.Context, registerCtx.HostManager.GetConfig(), registerCtx.VirtualManager.GetConfig(), schema.FromAPIVersionAndKind(m.FromVirtualCluster.APIVersion, m.FromVirtualCluster.Kind))
				if err != nil {
					klog.Fatalf("Error syncronizing CRD %s(%s) from the host cluster into vcluster: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
				}
				// }

				for _, c := range m.FromVirtualCluster.SyncBack {
					// if !plugin.Scheme.Recognizes(schema.FromAPIVersionAndKind(m.FromVirtualCluster.APIVersion, m.FromVirtualCluster.Kind)) {
					_, _, err := translate.EnsureCRDFromPhysicalCluster(registerCtx.Context, registerCtx.HostManager.GetConfig(), registerCtx.VirtualManager.GetConfig(), schema.FromAPIVersionAndKind(c.APIVersion, c.Kind))
					if err != nil {
						klog.Fatalf("Error syncronizing CRD %s(%s) from the host cluster into vcluster: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
					}
					// }
				}
			}
		}

		// create a single name cache
		nc, err := namecache.NewNameCache(registerCtx.Context, registerCtx.VirtualManager, configuration)
		if err != nil {
			klog.Fatal("Error seting up namecache for a mapping ", err)
		}

		forceSyncSecrets := []syncer.ForceSyncConfig{}
		forceSyncConfigmaps := []syncer.ForceSyncConfig{}

		for _, m := range configuration.Mappings {
			if m.FromVirtualCluster != nil {
				s, err := syncer.CreateFromVirtualSyncer(registerCtx, m.FromVirtualCluster, nc)
				if err != nil {
					klog.Fatalf("Error creating %s(%s) syncer: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
				}

				err = plugin.Register(s)
				if err != nil {
					klog.Fatalf("Error registering %s(%s) syncer: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
				}

				// check if this mapping uses Secret/Configmap sync
				for _, p := range m.FromVirtualCluster.Patches {
					if p.Sync != nil && p.Sync.Secret != nil && *p.Sync.Secret {
						forceSyncSecrets = append(forceSyncSecrets, syncer.ForceSyncConfig{
							Parent: *m.FromVirtualCluster,
							Patch:  *p,
						})
					}
					if p.Sync != nil && p.Sync.ConfigMap != nil && *p.Sync.ConfigMap {
						forceSyncConfigmaps = append(forceSyncConfigmaps, syncer.ForceSyncConfig{
							Parent: *m.FromVirtualCluster,
							Patch:  *p,
						})
					}
				}

				for _, c := range m.FromVirtualCluster.SyncBack {
					backSyncer, err := syncer.CreateBackSyncer(registerCtx, c, m.FromVirtualCluster, nc)
					if err != nil {
						klog.Fatalf("Error creating %s(%s) backsyncer: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
					}

					err = plugin.Register(backSyncer)
					if err != nil {
						klog.Fatalf("Error registering %s(%s) syncer: %v", m.FromVirtualCluster.Kind, m.FromVirtualCluster.APIVersion, err)
					}
				}
			}
		}

		if len(forceSyncSecrets) > 0 {
			s, err := syncer.CreateForceSyncController(registerCtx, corev1.SchemeGroupVersion.WithKind("Secret"), forceSyncSecrets, nc)
			if err != nil {
				klog.Fatalf("Error creating Secret ForceSyncController : %v", err)
			}
			err = plugin.Register(s)
			if err != nil {
				klog.Fatalf("Error registering Secret ForceSyncController: %v", err)
			}
		}
		if len(forceSyncConfigmaps) > 0 {
			s, err := syncer.CreateForceSyncController(registerCtx, corev1.SchemeGroupVersion.WithKind("ConfigMap"), forceSyncConfigmaps, nc)
			if err != nil {
				klog.Fatalf("Error creating ConfigMap ForceSyncController: %v", err)
			}
			err = plugin.Register(s)
			if err != nil {
				klog.Fatalf("Error registering ConfigMap ForceSyncController: %v", err)
			}
		}
	}

	// start plugin
	err := plugin.Start()
	if err != nil {
		klog.Fatalf("Error starting plugin: %v", err)
	}
}
