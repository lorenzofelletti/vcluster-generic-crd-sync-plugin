// Modified by Lorenzo Felletti (2025) under Apache 2.0.
// Original code from vcluster-generic-crd-sync-plugin by Loft Labs.

package syncer

import (
	"fmt"
	"regexp"

	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/config"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/namecache"
	patchesregex "github.com/loft-sh/vcluster-generic-crd-plugin/pkg/patches/regex"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/plugin"
	"github.com/loft-sh/vcluster/pkg/syncer/synccontext"
	"github.com/loft-sh/vcluster/pkg/syncer/translator"
	syncertypes "github.com/loft-sh/vcluster/pkg/syncer/types"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func CreateFromVirtualSyncer(ctx *synccontext.RegisterContext, config *config.FromVirtualCluster, nc namecache.NameCache) (syncertypes.Syncer, error) {
	obj := &unstructured.Unstructured{}
	obj.SetKind(config.Kind)
	obj.SetAPIVersion(config.APIVersion)

	err := validateFromVirtualConfig(config)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration for %s(%s) mapping: %v", config.Kind, config.APIVersion, err)
	}

	var selector labels.Selector
	if config.Selector != nil {
		selector, err = metav1.LabelSelectorAsSelector(metav1.SetAsLabelSelector(config.Selector.LabelSelector))
		if err != nil {
			return nil, fmt.Errorf("invalid selector in configuration for %s(%s) mapping: %v", config.Kind, config.APIVersion, err)
		}
	}

	statusIsSubresource := true
	// TODO: [low priority] check if config.Kind + config.APIVersion has status subresource

	mapper := &virtualToHostMapper{
		namespace:       ctx.Config.HostNamespace,
		targetNamespace: ctx.Config.HostNamespace,
	}

	return &fromVirtualController{
		GenericTranslator: translator.NewGenericTranslator(ctx, config.Kind+"-from-virtual-syncer", obj, mapper),
		patcher: &patcher{
			fromClient:          ctx.VirtualManager.GetClient(),
			toClient:            ctx.HostManager.GetClient(),
			statusIsSubresource: statusIsSubresource,
		},
		gvk:             schema.FromAPIVersionAndKind(config.APIVersion, config.Kind),
		config:          config,
		nameCache:       nc,
		selector:        selector,
		targetNamespace: ctx.Config.HostNamespace,
		mapper:          mapper,
	}, nil
}

type fromVirtualController struct {
	syncertypes.GenericTranslator

	patcher *patcher
	gvk     schema.GroupVersionKind

	config          *config.FromVirtualCluster
	nameCache       namecache.NameCache
	selector        labels.Selector
	targetNamespace string
	mapper          *virtualToHostMapper
}

func (f *fromVirtualController) SyncDown(ctx *synccontext.SyncContext, vObj client.Object) (ctrl.Result, error) {
	// check if selector matches
	if isControlled(vObj) || !f.objectMatches(vObj) {
		return ctrl.Result{}, nil
	}

	// apply object to physical cluster
	ctx.Log.Infof("Create physical %s %s/%s, since it is missing, but virtual object exists", f.config.Kind, vObj.GetNamespace(), vObj.GetName())
	pName := f.mapper.VirtualToHost(ctx, types.NamespacedName{Name: vObj.GetName(), Namespace: vObj.GetNamespace()}, vObj)
	pObj := translate.HostMetadata(vObj, pName)
	_, err := f.patcher.ApplyPatches(ctx.Context, vObj, pObj, f.config.Patches, f.config.ReversePatches, func(vObj client.Object) (client.Object, error) {
		return pObj, nil
	}, &virtualToHostNameResolver{namespace: vObj.GetNamespace(), targetNamespace: f.targetNamespace})
	if err != nil {
		f.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{}, fmt.Errorf("error applying patches: %v", err)
	}

	return ctrl.Result{}, nil
}
func (f *fromVirtualController) isExcluded(pObj client.Object) bool {
	labels := pObj.GetLabels()
	return labels == nil || labels[controlledByLabel] != f.getControllerID()
}

// Implement Syncer interface
func (f *fromVirtualController) Syncer() syncertypes.Sync[client.Object] {
	return f
}

// SyncToHost is called when a virtual object was created and needs to be synced down to the physical cluster
func (f *fromVirtualController) SyncToHost(ctx *synccontext.SyncContext, event *synccontext.SyncToHostEvent[client.Object]) (ctrl.Result, error) {
	return f.SyncDown(ctx, event.Virtual)
}

// Sync is called to sync a virtual object with a physical object
func (f *fromVirtualController) Sync(ctx *synccontext.SyncContext, event *synccontext.SyncEvent[client.Object]) (ctrl.Result, error) {
	vObj := event.Virtual
	pObj := event.Host

	if isControlled(vObj) || f.isExcluded(pObj) {
		return ctrl.Result{}, nil
	} else if !f.objectMatches(vObj) {
		ctx.Log.Infof("delete physical %s %s/%s, because it is not used anymore", f.config.Kind, pObj.GetNamespace(), pObj.GetName())
		err := ctx.HostClient.Delete(ctx, pObj)
		if err != nil {
			ctx.Log.Infof("error deleting physical %s %s/%s in physical cluster: %v", f.config.Kind, pObj.GetNamespace(), pObj.GetName(), err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// apply reverse patches
	result, err := f.patcher.ApplyReversePatches(ctx.Context, vObj, pObj, f.config.ReversePatches, &hostToVirtualNameResolver{nameCache: f.nameCache, gvk: f.gvk})
	if err != nil {
		if kerrors.IsInvalid(err) {
			ctx.Log.Infof("Warning: this message could indicate a timing issue with no significant impact, or a bug. Please report this if your resource never reaches the expected state. Error message: failed to patch virtual %s %s/%s: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
			// this happens when some field is being removed shortly after being added, which suggest it's a timing issue
			// it doesn't seem to have any negative consequence besides the logged error message
			return ctrl.Result{Requeue: true}, nil
		}

		f.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error syncing to virtual cluster: %v", err)
		return ctrl.Result{}, fmt.Errorf("failed to patch virtual %s %s/%s: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
	} else if result == controllerutil.OperationResultUpdated || result == controllerutil.OperationResultUpdatedStatus || result == controllerutil.OperationResultUpdatedStatusOnly {
		// a change will trigger reconciliation anyway, and at that point we can make
		// a more accurate updates(reverse patches) to the virtual resource
		return ctrl.Result{}, nil
	}

	// apply patches
	pName := f.mapper.VirtualToHost(ctx, types.NamespacedName{Name: vObj.GetName(), Namespace: vObj.GetNamespace()}, vObj)
	updatedPObj := translate.HostMetadata(vObj, pName)
	_, err = f.patcher.ApplyPatches(ctx.Context, vObj, updatedPObj, f.config.Patches, f.config.ReversePatches, func(vObj client.Object) (client.Object, error) {
		return updatedPObj, nil
	}, &virtualToHostNameResolver{namespace: vObj.GetNamespace(), targetNamespace: f.targetNamespace})
	if err != nil {
		if kerrors.IsInvalid(err) {
			ctx.Log.Infof("Warning: this message could indicate a timing issue with no significant impact, or a bug. Please report this if your resource never reaches the expected state. Error message: failed to patch physical %s %s/%s: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
			// this happens when some field is being removed shortly after being added, which suggest it's a timing issue
			// it doesn't seem to have any negative consequence besides the logged error message
			return ctrl.Result{Requeue: true}, nil
		}

		f.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{}, fmt.Errorf("error applying patches: %v", err)
	}

	return ctrl.Result{}, nil
}

func (f *fromVirtualController) SyncToVirtual(ctx *synccontext.SyncContext, event *synccontext.SyncToVirtualEvent[client.Object]) (ctrl.Result, error) {
	isManaged, err := f.mapper.IsManaged(ctx, event.Host)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !isManaged || f.isExcluded(event.Host) {
		return ctrl.Result{}, nil
	}

	// delete physical object because virtual one is missing
	err = ctx.HostClient.Delete(ctx, event.Host)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (f *fromVirtualController) getControllerID() string {
	if f.config.ID != "" {
		return f.config.ID
	}
	return plugin.GetPluginName()
}

func isControlled(obj client.Object) bool {
	return obj.GetLabels() != nil && obj.GetLabels()[controlledByLabel] != ""
}

func (f *fromVirtualController) objectMatches(obj client.Object) bool {
	return f.selector == nil || !f.selector.Matches(labels.Set(obj.GetLabels()))
}

type virtualToHostNameResolver struct {
	namespace       string
	targetNamespace string
}

func (r *virtualToHostNameResolver) TranslateName(name string, regex *regexp.Regexp, _ string) (string, error) {
	return r.TranslateNameWithNamespace(name, r.namespace, regex, "")
}

func (r *virtualToHostNameResolver) TranslateNameWithNamespace(name string, namespace string, regex *regexp.Regexp, _ string) (string, error) {
	if regex != nil {
		return patchesregex.ProcessRegex(regex, name, func(name, ns string) types.NamespacedName {
			// if the regex match doesn't contain namespace - use the namespace set in this resolver
			if ns == "" {
				ns = namespace
			}
			return types.NamespacedName{Namespace: r.targetNamespace, Name: translate.SingleNamespaceHostName(name, ns, translate.VClusterName)}
		}), nil
	} else {
		return translate.SingleNamespaceHostName(name, namespace, translate.VClusterName), nil
	}
}

func (r *virtualToHostNameResolver) TranslateLabelExpressionsSelector(selector *metav1.LabelSelector) (*metav1.LabelSelector, error) {
	var s *metav1.LabelSelector
	if selector != nil {
		s = &metav1.LabelSelector{MatchLabels: map[string]string{}}
		for k, v := range selector.MatchLabels {
			s.MatchLabels[translate.HostLabelNamespace(k)] = v
		}
		if len(selector.MatchExpressions) > 0 {
			s.MatchExpressions = []metav1.LabelSelectorRequirement{}
			for i, r := range selector.MatchExpressions {
				s.MatchExpressions[i] = metav1.LabelSelectorRequirement{
					Key:      translate.HostLabelNamespace(r.Key),
					Operator: r.Operator,
					Values:   r.Values,
				}
			}
		}
		s.MatchLabels[translate.NamespaceLabel] = r.namespace
		s.MatchLabels[translate.MarkerLabel] = translate.VClusterName
	}
	return s, nil
}

func (r *virtualToHostNameResolver) TranslateLabelKey(key string) (string, error) {
	return translate.HostLabelNamespace(key), nil
}

func (r *virtualToHostNameResolver) TranslateLabelSelector(selector map[string]string) (map[string]string, error) {
	s := map[string]string{}
	if selector != nil {
		for k, v := range selector {
			s[translate.HostLabelNamespace(k)] = v
		}
		s[translate.NamespaceLabel] = r.namespace
		s[translate.MarkerLabel] = translate.VClusterName
	}
	return s, nil
}

func (r *virtualToHostNameResolver) TranslateNamespaceRef(namespace string) (string, error) {
	return r.targetNamespace, nil
}

type hostToVirtualNameResolver struct {
	gvk schema.GroupVersionKind

	nameCache namecache.NameCache
}

func (r *hostToVirtualNameResolver) TranslateName(name string, regex *regexp.Regexp, path string) (string, error) {
	var n types.NamespacedName
	if regex != nil {
		return patchesregex.ProcessRegex(regex, name, func(name, namespace string) types.NamespacedName {
			if path == "" {
				return r.nameCache.ResolveName(r.gvk, name)
			} else {
				return r.nameCache.ResolveNamePath(r.gvk, name, path)
			}
		}), nil
	} else {
		if path == "" {
			n = r.nameCache.ResolveName(r.gvk, name)
		} else {
			n = r.nameCache.ResolveNamePath(r.gvk, name, path)
		}
	}
	if n.Name == "" {
		return "", fmt.Errorf("could not translate %s host resource name to vcluster resource name", name)
	}

	return n.Name, nil
}
func (r *hostToVirtualNameResolver) TranslateNameWithNamespace(name string, namespace string, regex *regexp.Regexp, path string) (string, error) {
	return "", fmt.Errorf("translation not supported from host to virtual object")
}
func (r *hostToVirtualNameResolver) TranslateLabelKey(key string) (string, error) {
	return "", fmt.Errorf("translation not supported from host to virtual object")
}
func (r *hostToVirtualNameResolver) TranslateLabelExpressionsSelector(selector *metav1.LabelSelector) (*metav1.LabelSelector, error) {
	return nil, fmt.Errorf("translation not supported from host to virtual object")
}
func (r *hostToVirtualNameResolver) TranslateLabelSelector(selector map[string]string) (map[string]string, error) {
	return nil, fmt.Errorf("translation not supported from host to virtual object")
}
func (r *hostToVirtualNameResolver) TranslateNamespaceRef(namespace string) (string, error) {
	return "", fmt.Errorf("translation not supported from host to virtual object")
}

func validateFromVirtualConfig(config *config.FromVirtualCluster) error {
	for _, p := range append(config.Patches, config.ReversePatches...) {
		if p.Regex != "" {
			parsed, err := patchesregex.PrepareRegex(p.Regex)
			if err != nil {
				return fmt.Errorf("invalid Regex: %v", err)
			}
			p.ParsedRegex = parsed
		}
	}
	return nil
}

// virtualToHostMapper implements the Mapper interface
type virtualToHostMapper struct {
	namespace       string
	targetNamespace string
}

func (m *virtualToHostMapper) Migrate(_ *synccontext.RegisterContext, _ synccontext.Mapper) error {
	return nil
}

func (m *virtualToHostMapper) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{}
}

func (m *virtualToHostMapper) VirtualToHost(ctx *synccontext.SyncContext, req types.NamespacedName, vObj client.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.targetNamespace,
		Name:      translate.SingleNamespaceHostName(req.Name, req.Namespace, translate.VClusterName),
	}
}

func (m *virtualToHostMapper) HostToVirtual(_ *synccontext.SyncContext, req types.NamespacedName, _ client.Object) types.NamespacedName {
	return types.NamespacedName{}
}

func (m *virtualToHostMapper) IsManaged(ctx *synccontext.SyncContext, pObj client.Object) (bool, error) {
	// check if the object has the managed-by label
	return pObj.GetLabels() != nil && pObj.GetLabels()[translate.MarkerLabel] == translate.VClusterName, nil
}
