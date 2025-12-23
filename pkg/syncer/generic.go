// Modified by Lorenzo Felletti (2025) under Apache 2.0.
// Original code from vcluster-generic-crd-sync-plugin by Loft Labs.

package syncer

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/lorenzofelletti/vcluster-generic-crd-plugin/pkg/config"
	"github.com/lorenzofelletti/vcluster-generic-crd-plugin/pkg/patches"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var (
	fieldManager = "vcluster-syncer"

	controlledByLabel = "vcluster.loft.sh/controlled-by"
)

type patcher struct {
	fromClient client.Client
	toClient   client.Client

	statusIsSubresource bool
}

func (s *patcher) ApplyPatches(ctx context.Context, fromObj, toObj client.Object, patchesConfig, reversePatchesConfig []*config.Patch, translateMetadata func(vObj client.Object) (client.Object, error), nameResolver patches.NameResolver) (client.Object, error) {
	logger := logr.FromContextOrDiscard(ctx)

	translatedObject, err := translateMetadata(fromObj)
	if err != nil {
		return nil, errors.Wrap(err, "translate object")
	}

	toObjBase, err := toUnstructured(translatedObject)
	if err != nil {
		return nil, err
	}
	toObjCopied := toObjBase.DeepCopy()

	// apply patches on from object
	err = patches.ApplyPatches(toObjCopied, toObj, patchesConfig, reversePatchesConfig, nameResolver)
	if err != nil {
		return nil, fmt.Errorf("error applying patches: %v", err)
	}

	// compare status
	if s.statusIsSubresource {
		_, hasAfterStatus, err := unstructured.NestedFieldCopy(toObjCopied.Object, "status")
		if err != nil {
			return nil, err
		}

		// always apply status if it's there
		if hasAfterStatus {
			logger.Info("Apply status of object during patching", "name", toObjCopied.GetName())
			err = s.toClient.Status().Patch(ctx, toObjCopied.DeepCopy(), client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
			if err != nil {
				return nil, errors.Wrap(err, "apply status")
			}
		}

		if hasAfterStatus {
			unstructured.RemoveNestedField(toObjCopied.Object, "status")
		}
	}

	// always apply object
	logger.Info("Apply object during patching", "name", toObjCopied.GetName())
	outObject := toObjCopied.DeepCopy()
	err = s.toClient.Patch(ctx, outObject, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
	if err != nil {
		return nil, errors.Wrap(err, "apply object")
	}

	return outObject, nil
}

func (s *patcher) ApplyReversePatches(ctx context.Context, fromObj, otherObj client.Object, reversePatchConfig []*config.Patch, nameResolver patches.NameResolver) (controllerutil.OperationResult, error) {
	logger := logr.FromContextOrDiscard(ctx)

	originalUnstructured, err := toUnstructured(fromObj)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}
	fromCopied := originalUnstructured.DeepCopy()

	// apply patches on from object
	err = patches.ApplyPatches(fromCopied, otherObj, reversePatchConfig, nil, nameResolver)
	if err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("error applying reverse patches: %v", err)
	}

	// compare status
	if s.statusIsSubresource {
		beforeStatus, hasBeforeStatus, err := unstructured.NestedFieldCopy(originalUnstructured.Object, "status")
		if err != nil {
			return controllerutil.OperationResultNone, err
		}
		afterStatus, hasAfterStatus, err := unstructured.NestedFieldCopy(fromCopied.Object, "status")
		if err != nil {
			return controllerutil.OperationResultNone, err
		}

		// update status
		if (hasBeforeStatus || hasAfterStatus) && !equality.Semantic.DeepEqual(beforeStatus, afterStatus) {
			logger.Info("Update status of object during reverse patching", "name", fromCopied.GetName())
			err = s.fromClient.Status().Update(ctx, fromCopied)
			if err != nil {
				return controllerutil.OperationResultNone, errors.Wrap(err, "update reverse status")
			}

			return controllerutil.OperationResultUpdatedStatusOnly, nil
		}

		if hasBeforeStatus {
			unstructured.RemoveNestedField(originalUnstructured.Object, "status")
		}
		if hasAfterStatus {
			unstructured.RemoveNestedField(fromCopied.Object, "status")
		}
	}

	// compare rest of the object
	if !equality.Semantic.DeepEqual(originalUnstructured, fromCopied) {
		logger.Info("Update object during reverse patching", "name", fromCopied.GetName())
		err = s.fromClient.Update(ctx, fromCopied)
		if err != nil {
			return controllerutil.OperationResultNone, errors.Wrap(err, "update reverse")
		}

		return controllerutil.OperationResultUpdated, nil
	}

	return controllerutil.OperationResultNone, nil
}

func toUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	fromCopied, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj.DeepCopyObject())
	if err != nil {
		return nil, err
	}

	return &unstructured.Unstructured{Object: fromCopied}, nil
}
