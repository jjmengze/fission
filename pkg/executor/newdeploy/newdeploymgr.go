/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package newdeploy

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dchest/uniuri"
	"github.com/fission/fission/pkg/throttler"
	"github.com/fission/fission/pkg/utils"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apiv1 "k8s.io/api/core/v1"
	k8sErrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/fission.io/v1"
	"github.com/fission/fission/pkg/crd"
	"github.com/fission/fission/pkg/executor/fscache"
	fetcherConfig "github.com/fission/fission/pkg/fetcher/config"
	"github.com/fission/fission/pkg/types"
)

type (
	NewDeploy struct {
		logger *zap.Logger

		kubernetesClient *kubernetes.Clientset
		fissionClient    *crd.FissionClient
		crdClient        *rest.RESTClient
		instanceID       string
		fetcherConfig    *fetcherConfig.Config

		runtimeImagePullPolicy apiv1.PullPolicy
		namespace              string
		useIstio               bool
		collectorEndpoint      string

		fsCache *fscache.FunctionServiceCache // cache funcSvc's by function, address and pod name

		throttler      *throttler.Throttler
		funcStore      k8sCache.Store
		funcController k8sCache.Controller

		envStore      k8sCache.Store
		envController k8sCache.Controller

		idlePodReapTime time.Duration
	}
)

func MakeNewDeploy(
	logger *zap.Logger,
	fissionClient *crd.FissionClient,
	kubernetesClient *kubernetes.Clientset,
	crdClient *rest.RESTClient,
	namespace string,
	fetcherConfig *fetcherConfig.Config,
	instanceID string,
) *NewDeploy {
	enableIstio := false
	if len(os.Getenv("ENABLE_ISTIO")) > 0 {
		istio, err := strconv.ParseBool(os.Getenv("ENABLE_ISTIO"))
		if err != nil {
			logger.Error("failed to parse 'ENABLE_ISTIO', set to false", zap.Error(err))
		}
		enableIstio = istio
	}

	nd := &NewDeploy{
		logger: logger.Named("new_deploy"),

		fissionClient:    fissionClient,
		kubernetesClient: kubernetesClient,
		crdClient:        crdClient,
		instanceID:       instanceID,

		namespace: namespace,
		fsCache:   fscache.MakeFunctionServiceCache(logger),
		throttler: throttler.MakeThrottler(1 * time.Minute),

		fetcherConfig:          fetcherConfig,
		runtimeImagePullPolicy: utils.GetImagePullPolicy(os.Getenv("RUNTIME_IMAGE_PULL_POLICY")),
		useIstio:               enableIstio,

		idlePodReapTime: 2 * time.Minute,
	}

	if nd.crdClient != nil {
		fnStore, fnController := nd.initFuncController()
		nd.funcStore = fnStore
		nd.funcController = fnController

		envStore, envController := nd.initEnvController()
		nd.envStore = envStore
		nd.envController = envController
	}

	return nd
}

func (deploy *NewDeploy) Run(ctx context.Context) {
	//go deploy.service()
	go deploy.funcController.Run(ctx.Done())
	go deploy.envController.Run(ctx.Done())
	go deploy.idleObjectReaper()
}

func (deploy *NewDeploy) initFuncController() (k8sCache.Store, k8sCache.Controller) {
	resyncPeriod := 30 * time.Second
	listWatch := k8sCache.NewListWatchFromClient(deploy.crdClient, "functions", metav1.NamespaceAll, fields.Everything())
	store, controller := k8sCache.NewInformer(listWatch, &fv1.Function{}, resyncPeriod, k8sCache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fn := obj.(*fv1.Function)
			_, err := deploy.createFunction(fn, true)
			if err != nil {
				deploy.logger.Error("error eager creating function",
					zap.Error(err),
					zap.Any("function", fn))
			}
		},
		DeleteFunc: func(obj interface{}) {
			fn := obj.(*fv1.Function)
			err := deploy.deleteFunction(fn)
			if err != nil {
				deploy.logger.Error("error deleting function",
					zap.Error(err),
					zap.Any("function", fn))
			}
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldFn := oldObj.(*fv1.Function)
			newFn := newObj.(*fv1.Function)
			err := deploy.updateFunction(oldFn, newFn)
			if err != nil {
				deploy.logger.Error("error updating function",
					zap.Error(err),
					zap.Any("old_function", oldFn),
					zap.Any("new_function", newFn))
			}
		},
	})
	return store, controller
}

func (deploy *NewDeploy) initEnvController() (k8sCache.Store, k8sCache.Controller) {
	resyncPeriod := 30 * time.Second
	listWatch := k8sCache.NewListWatchFromClient(deploy.crdClient, "environments", metav1.NamespaceAll, fields.Everything())
	store, controller := k8sCache.NewInformer(listWatch, &fv1.Environment{}, resyncPeriod, k8sCache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) {},
		DeleteFunc: func(obj interface{}) {},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			newEnv := newObj.(*fv1.Environment)
			oldEnv := oldObj.(*fv1.Environment)
			// Currently only an image update in environment calls for function's deployment recreation. In future there might be more attributes which would want to do it
			if oldEnv.Spec.Runtime.Image != newEnv.Spec.Runtime.Image {
				deploy.logger.Debug("Updating all function of the environment that changed, old env:", zap.Any("environment", oldEnv))
				funcs := deploy.getEnvFunctions(&newEnv.Metadata)
				for _, f := range funcs {
					function, err := deploy.fissionClient.Functions(f.Metadata.Namespace).Get(f.Metadata.Name)
					if err != nil {
						deploy.logger.Error("Error getting function", zap.Error(err), zap.Any("function", function))
						continue
					}
					err = deploy.updateFuncDeployment(function, newEnv)
					if err != nil {
						deploy.logger.Error("Error updating function", zap.Error(err), zap.Any("function", function))
						continue
					}
				}
			}
		},
	})
	return store, controller
}

func (deploy *NewDeploy) getEnvFunctions(m *metav1.ObjectMeta) []fv1.Function {
	funcList, err := deploy.fissionClient.Functions(m.Namespace).List(metav1.ListOptions{})
	if err != nil {
		deploy.logger.Error("Error getting functions for env", zap.Error(err), zap.Any("environment", m))
	}
	relatedFunctions := make([]fv1.Function, 0)
	for _, f := range funcList.Items {
		if (f.Spec.Environment.Name == m.Name) && (f.Spec.Environment.Namespace == m.Namespace) {
			relatedFunctions = append(relatedFunctions, f)
		}
	}
	return relatedFunctions
}

func (deploy *NewDeploy) GetFuncSvc(ctx context.Context, metadata *metav1.ObjectMeta) (*fscache.FuncSvc, error) {
	fn, err := deploy.fissionClient.Functions(metadata.Namespace).Get(metadata.Name)
	if err != nil {
		return nil, err
	}
	return deploy.createFunction(fn, false)
}

// RefreshFuncPods deleted pods related to the function so that new pods are replenished
func (deploy *NewDeploy) RefreshFuncPods(logger *zap.Logger, f fv1.Function) error {

	env, err := deploy.fissionClient.Environments(f.Spec.Environment.Namespace).Get(f.Spec.Environment.Name)
	if err != nil {
		return err
	}

	funcLabels := deploy.getDeployLabels(f.Metadata, metav1.ObjectMeta{
		Name:      f.Spec.Environment.Name,
		Namespace: f.Spec.Environment.Namespace,
		UID:       env.Metadata.UID,
	})

	dep, err := deploy.kubernetesClient.AppsV1().Deployments(metav1.NamespaceAll).List(metav1.ListOptions{
		LabelSelector: labels.Set(funcLabels).AsSelector().String(),
	})

	if err != nil {
		return err
	}

	patch := fmt.Sprintf(`{"spec" : {"template": {"spec":{"containers":[{"name": "%s", "env":[{"name": "%s", "value": "%s"}]}]}}}}`,
		f.Metadata.Name,
		fv1.LastUpdateTimestamp,
		time.Now().String())

	// Ideally there should be only one deployment but for now we rely on label/selector to ensure that condition
	for _, deployment := range dep.Items {
		_, err := deploy.kubernetesClient.AppsV1().Deployments(deployment.ObjectMeta.Namespace).Patch(deployment.ObjectMeta.Name,
			k8sTypes.StrategicMergePatchType,
			[]byte(patch))
		if err != nil {
			return err
		}
	}
	return nil
}

func (deploy *NewDeploy) createFunction(fn *fv1.Function, firstcreate bool) (*fscache.FuncSvc, error) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy {
		return nil, nil
	}

	fsvcObj, err := deploy.throttler.RunOnce(string(fn.Metadata.UID), func(ableToCreate bool) (interface{}, error) {
		if ableToCreate {
			return deploy.fnCreate(fn, firstcreate)
		}
		return deploy.fsCache.GetByFunctionUID(fn.Metadata.UID)
	})

	if err != nil {
		e := "error updating service address entry for function"
		deploy.logger.Error(e,
			zap.Error(err),
			zap.String("function_name", fn.Metadata.Name),
			zap.String("function_namespace", fn.Metadata.Namespace))
		return nil, errors.Wrapf(err, "%s %s_%s", e, fn.Metadata.Name, fn.Metadata.Namespace)
	}

	fsvc, ok := fsvcObj.(*fscache.FuncSvc)
	if !ok {
		deploy.logger.Panic("receive unknown object while creating function - expected pointer of function service object")
	}

	return fsvc, err
}

func (deploy *NewDeploy) deleteFunction(fn *fv1.Function) error {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy {
		return nil
	}
	err := deploy.fnDelete(fn)
	if err != nil {
		err = errors.Wrapf(err, "error deleting kubernetes objects of function %v", fn.Metadata)
	}
	return err
}

func (deploy *NewDeploy) fnCreate(fn *fv1.Function, firstcreate bool) (*fscache.FuncSvc, error) {
	env, err := deploy.fissionClient.
		Environments(fn.Spec.Environment.Namespace).
		Get(fn.Spec.Environment.Name)
	if err != nil {
		return nil, err
	}

	objName := deploy.getObjName(fn)
	if !firstcreate {
		// retrieve back the previous obj name for later use.
		fsvc, err := deploy.fsCache.GetByFunctionUID(fn.Metadata.UID)
		if err != nil {
			return nil, err
		}
		objName = fsvc.Name
	}
	deployLabels := deploy.getDeployLabels(fn.Metadata, env.Metadata)

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := deploy.namespace
	if fn.Metadata.Namespace != metav1.NamespaceDefault {
		ns = fn.Metadata.Namespace
	}

	// Envoy(istio-proxy) returns 404 directly before istio pilot
	// propagates latest Envoy-specific configuration.
	// Since newdeploy waits for pods of deployment to be ready,
	// change the order of kubeObject creation (create service first,
	// then deployment) to take advantage of waiting time.
	svc, err := deploy.createOrGetSvc(deployLabels, objName, ns)
	if err != nil {
		deploy.logger.Error("error creating service", zap.Error(err), zap.String("service", objName))
		go deploy.cleanupNewdeploy(ns, objName)
		return nil, errors.Wrapf(err, "error creating service %v", objName)
	}
	svcAddress := fmt.Sprintf("%v.%v", svc.Name, svc.Namespace)

	depl, err := deploy.createOrGetDeployment(fn, env, objName, deployLabels, ns, firstcreate)
	if err != nil {
		deploy.logger.Error("error creating deployment", zap.Error(err), zap.String("deployment", objName))
		go deploy.cleanupNewdeploy(ns, objName)
		return nil, errors.Wrapf(err, "error creating deployment %v", objName)
	}

	hpa, err := deploy.createOrGetHpa(objName, &fn.Spec.InvokeStrategy.ExecutionStrategy, depl)
	if err != nil {
		deploy.logger.Error("error creating HPA", zap.Error(err), zap.String("hpa", objName))
		go deploy.cleanupNewdeploy(ns, objName)
		return nil, errors.Wrapf(err, "error creating the HPA %v", objName)
	}

	kubeObjRefs := []apiv1.ObjectReference{
		{
			//obj.TypeMeta.Kind does not work hence this, needs investigation and a fix
			Kind:            "deployment",
			Name:            depl.ObjectMeta.Name,
			APIVersion:      depl.TypeMeta.APIVersion,
			Namespace:       depl.ObjectMeta.Namespace,
			ResourceVersion: depl.ObjectMeta.ResourceVersion,
			UID:             depl.ObjectMeta.UID,
		},
		{
			Kind:            "service",
			Name:            svc.ObjectMeta.Name,
			APIVersion:      svc.TypeMeta.APIVersion,
			Namespace:       svc.ObjectMeta.Namespace,
			ResourceVersion: svc.ObjectMeta.ResourceVersion,
			UID:             svc.ObjectMeta.UID,
		},
		{
			Kind:            "horizontalpodautoscaler",
			Name:            hpa.ObjectMeta.Name,
			APIVersion:      hpa.TypeMeta.APIVersion,
			Namespace:       hpa.ObjectMeta.Namespace,
			ResourceVersion: hpa.ObjectMeta.ResourceVersion,
			UID:             hpa.ObjectMeta.UID,
		},
	}

	fsvc := &fscache.FuncSvc{
		Name:              objName,
		Function:          &fn.Metadata,
		Environment:       env,
		Address:           svcAddress,
		KubernetesObjects: kubeObjRefs,
		Executor:          fscache.NEWDEPLOY,
	}

	_, err = deploy.fsCache.Add(*fsvc)
	if err != nil {
		deploy.logger.Error("error adding function to cache", zap.Error(err), zap.Any("function", fsvc.Function))
		return fsvc, err
	}
	return fsvc, nil
}

func (deploy *NewDeploy) updateFunction(oldFn *fv1.Function, newFn *fv1.Function) error {

	if oldFn.Metadata.ResourceVersion == newFn.Metadata.ResourceVersion {
		return nil
	}

	// Ignoring updates to functions which are not of NewDeployment type
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy {
		return nil
	}

	// Executor type is no longer New Deployment
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeNewdeploy {
		deploy.logger.Info("function does not use new deployment executor anymore, deleting resources",
			zap.Any("function", newFn))
		// IMP - pass the oldFn, as the new/modified function is not in cache
		return deploy.deleteFunction(oldFn)
	}

	// Executor type changed to New Deployment from something else
	if oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeNewdeploy &&
		newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeNewdeploy {
		deploy.logger.Info("function type changed to new deployment, creating resources",
			zap.Any("old_function", oldFn.Metadata),
			zap.Any("new_function", newFn.Metadata))
		_, err := deploy.createFunction(newFn, true)
		if err != nil {
			deploy.updateStatus(oldFn, err, "error changing the function's type to newdeploy")
		}
		return err
	}

	deployChanged := false

	if oldFn.Spec.InvokeStrategy != newFn.Spec.InvokeStrategy {

		// to support backward compatibility, if the function was created in default ns, we fall back to creating the
		// deployment of the function in fission-function ns, so cleaning up resources there
		ns := deploy.namespace
		if newFn.Metadata.Namespace != metav1.NamespaceDefault {
			ns = newFn.Metadata.Namespace
		}

		fsvc, err := deploy.fsCache.GetByFunctionUID(newFn.Metadata.UID)
		if err != nil {
			err = errors.Wrapf(err, "error updating function due to unable to find function service cache: %v", oldFn)
			return err
		}

		hpa, err := deploy.getHpa(ns, fsvc.Name)
		if err != nil {
			deploy.updateStatus(oldFn, err, "error getting HPA while updating function")
			return err
		}

		hpaChanged := false

		if newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale {
			replicas := int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)
			hpa.Spec.MinReplicas = &replicas
			hpaChanged = true
		}

		if newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale {
			hpa.Spec.MaxReplicas = int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale)
			hpaChanged = true
		}

		if newFn.Spec.InvokeStrategy.ExecutionStrategy.TargetCPUPercent != oldFn.Spec.InvokeStrategy.ExecutionStrategy.TargetCPUPercent {
			targetCpupercent := int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.TargetCPUPercent)
			hpa.Spec.TargetCPUUtilizationPercentage = &targetCpupercent
			hpaChanged = true
		}

		if hpaChanged {
			err := deploy.updateHpa(hpa)
			if err != nil {
				deploy.updateStatus(oldFn, err, "error updating HPA while updating function")
				return err
			}
		}
	}

	if oldFn.Spec.Environment != newFn.Spec.Environment ||
		oldFn.Spec.Package.PackageRef != newFn.Spec.Package.PackageRef ||
		oldFn.Spec.Package.FunctionName != newFn.Spec.Package.FunctionName {
		deployChanged = true
	}

	// If length of slice has changed then no need to check individual elements
	if len(oldFn.Spec.Secrets) != len(newFn.Spec.Secrets) {
		deployChanged = true
	} else {
		for i, newSecret := range newFn.Spec.Secrets {
			if newSecret != oldFn.Spec.Secrets[i] {
				deployChanged = true
				break
			}
		}
	}
	if len(oldFn.Spec.ConfigMaps) != len(newFn.Spec.ConfigMaps) {
		deployChanged = true
	} else {
		for i, newConfig := range newFn.Spec.ConfigMaps {
			if newConfig != oldFn.Spec.ConfigMaps[i] {
				deployChanged = true
				break
			}
		}
	}

	if deployChanged == true {
		env, err := deploy.fissionClient.Environments(newFn.Spec.Environment.Namespace).
			Get(newFn.Spec.Environment.Name)
		if err != nil {
			deploy.updateStatus(oldFn, err, "failed to get environment while updating function")
			return err
		}
		return deploy.updateFuncDeployment(newFn, env)
	}

	return nil
}

func (deploy *NewDeploy) updateFuncDeployment(fn *fv1.Function, env *fv1.Environment) error {

	fsvc, err := deploy.fsCache.GetByFunctionUID(fn.Metadata.UID)
	if err != nil {
		err = errors.Wrapf(err, "error updating function due to unable to find function service cache: %v", fn)
		return err
	}
	fnObjName := fsvc.Name

	deployLabels := deploy.getDeployLabels(fn.Metadata, env.Metadata)
	deploy.logger.Info("updating deployment due to function/environment update",
		zap.String("deployment", fnObjName), zap.Any("function", fn.Metadata.Name))

	newDeployment, err := deploy.getDeploymentSpec(fn, env, fnObjName, deployLabels)
	if err != nil {
		deploy.updateStatus(fn, err, "failed to get new deployment spec while updating function")
		return err
	}

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := deploy.namespace
	if fn.Metadata.Namespace != metav1.NamespaceDefault {
		ns = fn.Metadata.Namespace
	}

	err = deploy.updateDeployment(newDeployment, ns)
	if err != nil {
		deploy.updateStatus(fn, err, "failed to update deployment while updating function")
		return err
	}

	return nil
}

func (deploy *NewDeploy) fnDelete(fn *fv1.Function) error {
	multierr := &multierror.Error{}

	// GetByFunction uses resource version as part of cache key, however,
	// the resource version in function metadata will be changed when a function
	// is deleted and cause newdeploy backend fails to delete the entry.
	// Use GetByFunctionUID instead of GetByFunction here to find correct
	// fsvc entry.
	fsvc, err := deploy.fsCache.GetByFunctionUID(fn.Metadata.UID)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("fsvc not found in cache: %v", fn.Metadata))
		return err
	}

	objName := fsvc.Name

	_, err = deploy.fsCache.DeleteOld(fsvc, time.Second*0)
	if err != nil {
		multierr = multierror.Append(multierr,
			errors.Wrap(err, fmt.Sprintf("error deleting the function from cache")))
	}

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns, so cleaning up resources there
	ns := deploy.namespace
	if fn.Metadata.Namespace != metav1.NamespaceDefault {
		ns = fn.Metadata.Namespace
	}

	err = deploy.cleanupNewdeploy(ns, objName)
	multierr = multierror.Append(multierr, err)

	return multierr.ErrorOrNil()
}

// getObjName returns a unique name for kubernetes objects of function
func (deploy *NewDeploy) getObjName(fn *fv1.Function) string {
	return strings.ToLower(fmt.Sprintf("newdeploy-%v-%v-%v", fn.Metadata.Name, fn.Metadata.Namespace, uniuri.NewLen(8)))
}

func (deploy *NewDeploy) getDeployLabels(fnMeta metav1.ObjectMeta, envMeta metav1.ObjectMeta) map[string]string {
	return map[string]string{
		types.EXECUTOR_INSTANCEID_LABEL: deploy.instanceID,
		types.EXECUTOR_TYPE:             fv1.ExecutorTypeNewdeploy,
		types.ENVIRONMENT_NAME:          envMeta.Name,
		types.ENVIRONMENT_NAMESPACE:     envMeta.Namespace,
		types.ENVIRONMENT_UID:           string(envMeta.UID),
		types.FUNCTION_NAME:             fnMeta.Name,
		types.FUNCTION_NAMESPACE:        fnMeta.Namespace,
		types.FUNCTION_UID:              string(fnMeta.UID),
	}
}

// updateKubeObjRefRV update the resource version of kubeObjectRef with
// given kind and return error if failed to find the reference.
func (deploy *NewDeploy) updateKubeObjRefRV(fsvc *fscache.FuncSvc, objKind string, rv string) error {
	kubeObjs := fsvc.KubernetesObjects
	for i, obj := range kubeObjs {
		if obj.Kind == objKind {
			kubeObjs[i].ResourceVersion = rv
			return nil
		}
	}
	fsvc.KubernetesObjects = kubeObjs
	return fmt.Errorf("error finding kubernetes object reference with kind: %v", objKind)
}

// updateStatus is a function which updates status of update.
// Current implementation only logs messages, in future it will update function status
func (deploy *NewDeploy) updateStatus(fn *fv1.Function, err error, message string) {
	deploy.logger.Error("function status update", zap.Error(err), zap.Any("function", fn), zap.String("message", message))
}

// IsValid does a get on the service address to ensure it's a valid service, then
// scale deployment to 1 replica if there are no available replicas for function.
// Return true if no error occurs, return false otherwise.
func (deploy *NewDeploy) IsValid(fsvc *fscache.FuncSvc) bool {
	service := strings.Split(fsvc.Address, ".")
	if len(service) == 0 {
		return false
	}

	_, err := deploy.kubernetesClient.CoreV1().Services(service[1]).Get(service[0], metav1.GetOptions{})
	if err != nil {
		deploy.logger.Error("error validating function service address", zap.String("function", fsvc.Function.Name), zap.Error(err))
		return false
	}

	deployObj := getDeploymentObj(fsvc.KubernetesObjects)
	if deployObj == nil {
		deploy.logger.Error("deployment obj for function does not exist", zap.String("function", fsvc.Function.Name))
		return false
	}

	currentDeploy, err := deploy.kubernetesClient.AppsV1().
		Deployments(deployObj.Namespace).Get(deployObj.Name, metav1.GetOptions{})
	if err != nil {
		deploy.logger.Error("error validating function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
		return false
	}

	// return directly when available replicas > 0
	if currentDeploy.Status.AvailableReplicas > 0 {
		return true
	}

	return false
}

// idleObjectReaper reaps objects after certain idle time
func (deploy *NewDeploy) idleObjectReaper() {

	pollSleep := time.Duration(deploy.idlePodReapTime)
	for {
		time.Sleep(pollSleep)

		envs, err := deploy.fissionClient.Environments(metav1.NamespaceAll).List(metav1.ListOptions{})
		if err != nil {
			deploy.logger.Fatal("failed to get environment list", zap.Error(err))
		}

		envList := make(map[k8sTypes.UID]struct{})
		for _, env := range envs.Items {
			envList[env.Metadata.UID] = struct{}{}
		}

		funcSvcs, err := deploy.fsCache.ListOld(deploy.idlePodReapTime)
		if err != nil {
			deploy.logger.Error("error reaping idle pods", zap.Error(err))
			continue
		}

		for _, fsvc := range funcSvcs {
			if fsvc.Executor != fscache.NEWDEPLOY {
				continue
			}

			// For function with the environment that no longer exists, executor
			// scales down the deployment as usual and prints log to notify user.
			if _, ok := envList[fsvc.Environment.Metadata.UID]; !ok {
				deploy.logger.Error("function environment no longer exists",
					zap.String("environment", fsvc.Environment.Metadata.Name),
					zap.String("function", fsvc.Name))
			}

			fn, err := deploy.fissionClient.Functions(fsvc.Function.Namespace).Get(fsvc.Function.Name)
			if err != nil {
				// Newdeploy manager handles the function delete event and clean cache/kubeobjs itself,
				// so we ignore the not found error for functions with newdeploy executor type here.
				if k8sErrs.IsNotFound(err) && fsvc.Executor == fscache.NEWDEPLOY {
					continue
				}
				deploy.logger.Error("error getting function", zap.Error(err), zap.String("function", fsvc.Function.Name))
				continue
			}

			deployObj := getDeploymentObj(fsvc.KubernetesObjects)
			if deployObj == nil {
				deploy.logger.Error("error finding function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				continue
			}

			currentDeploy, err := deploy.kubernetesClient.AppsV1().
				Deployments(deployObj.Namespace).Get(deployObj.Name, metav1.GetOptions{})
			if err != nil {
				deploy.logger.Error("error validating function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				continue
			}

			minScale := int32(fn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)

			// do nothing if the current replicas is already lower than minScale
			if *currentDeploy.Spec.Replicas <= minScale {
				continue
			}

			err = deploy.scaleDeployment(deployObj.Namespace, deployObj.Name, minScale)
			if err != nil {
				deploy.logger.Error("error scaling down function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
			}
		}
	}
}

func getDeploymentObj(kubeobjs []apiv1.ObjectReference) *apiv1.ObjectReference {
	for _, kubeobj := range kubeobjs {
		switch strings.ToLower(kubeobj.Kind) {
		case "deployment":
			return &kubeobj
		}
	}
	return nil
}

func (deploy *NewDeploy) scaleDeployment(deplNS string, deplName string, replicas int32) error {
	deploy.logger.Info("scaling deployment",
		zap.String("deployment", deplName),
		zap.String("namespace", deplNS),
		zap.Int32("replicas", replicas))
	_, err := deploy.kubernetesClient.AppsV1().Deployments(deplNS).UpdateScale(deplName, &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deplName,
			Namespace: deplNS,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: replicas,
		},
	})
	return err
}
