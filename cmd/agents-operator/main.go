package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"

	"os"
	"sync"
	"time"

	"github.com/antonmedv/expr"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/helm/pkg/strvals"
)

type AgentConfig struct {
	Agent []struct {
		Name      string `yaml:"name"`
		Container []struct {
			Resource []struct {
				Type    string `yaml:"type"`
				Request []struct {
					Value      string `yaml:"value"`
					UpperBound int    `yaml:"upper-bound"`
				} `yaml:"request"`
				Limit []struct {
					Value      string `yaml:"value"`
					UpperBound int    `yaml:"upper-bound"`
				} `yaml:"limit"`
			} `yaml:"resource"`
		} `yaml:"container"`
		ChartName string `yaml:"chartname"`
	} `yaml:"agent"`
}

var globalns = "accuknox-agents"
var agentConfig = "agents-operator-config"

func exprEval(valueExpr string, nodesCount int) int {

	env := map[string]interface{}{
		"n": nodesCount,
	}

	compiledExpr, err := expr.Compile(valueExpr, expr.Env(env))
	if err != nil {
		log.Error().Msg(err.Error())
		return -1
	}

	value, err := expr.Run(compiledExpr, env)
	if err != nil {
		log.Error().Msg(err.Error())
		return -1
	}

	valueInt, ok := value.(int)
	if !ok {
		log.Error().Msg(err.Error())
	}
	return valueInt
}

func getIndexForType(conf *AgentConfig, index int, restype string) int {
	for i, resource := range conf.Agent[index].Container[0].Resource {
		if resource.Type == restype {
			return i
		}
	}
	return -1
}

var configMapUpdated = true
var configMap *v1.ConfigMap

func updateAllAgents(clientset *kubernetes.Clientset, nodesCount int) {
	var err error
	if configMapUpdated {
		configMap, err = clientset.CoreV1().ConfigMaps(globalns).Get(context.TODO(), agentConfig, metav1.GetOptions{})
		if err != nil {
			log.Error().Msg(err.Error())
			return
		}
		configMapUpdated = false
	}

	var conf AgentConfig
	err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		log.Error().Msgf("Error parsing config: %v", err.Error())
		return
	}

	for i, resource := range conf.Agent {
		err = updateAgentResource(clientset, configMap, conf, i, nodesCount, resource.Name)
		if err != nil {
			log.Error().Msgf("Resource not updated: %v", err)
			return
		}
	}
}

func getReqLimit(restype string, conf AgentConfig, index int, nodesCount int) (int64, int64) {
	idx := getIndexForType(&conf, index, restype)
	if idx < 0 {
		log.Error().Msgf("could not getIndexForType")
		return -1, -1
	}
	mebibyte := 1
	if restype == "memory" {
		mebibyte = 1048576
	}

	limitUB := conf.Agent[index].Container[0].Resource[idx].Limit[1].UpperBound
	reqValueExpr := conf.Agent[index].Container[0].Resource[idx].Request[0].Value
	limitValueExpr := conf.Agent[index].Container[0].Resource[idx].Limit[0].Value

	req := int64(exprEval(reqValueExpr, nodesCount) * mebibyte)
	if req <= 0 {
		return -1, -1
	}
	limit := int64(exprEval(limitValueExpr, nodesCount) * mebibyte)
	if limit <= 0 {
		return -1, -1
	}

	if req > int64(limitUB*mebibyte) {
		req = int64(limitUB * mebibyte)
	}
	if limit > int64(limitUB*mebibyte) {
		limit = int64(limitUB * mebibyte)
	}
	if req > limit {
		req = limit
	}
	return req, limit
}

func updateAgentResource(clientset *kubernetes.Clientset, configMap *v1.ConfigMap, conf AgentConfig, index int, nodesCount int, agentName string) error {
	var err error

	cpuReq, cpuLimit := getReqLimit("cpu", conf, index, nodesCount)
	if cpuReq <= 0 || cpuLimit <= 0 {
		err = errors.New("could not get req limit for cpu")
		log.Error().Msgf("err=%v", err)
		return err
	}

	memReq, memLimit := getReqLimit("memory", conf, index, nodesCount)
	if memReq <= 0 || memLimit <= 0 {
		err = errors.New("could not get req limit for mem")
		log.Error().Msgf("err=%v", err)
		return err
	}

	deployment, err := clientset.AppsV1().Deployments(globalns).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		log.Error().Msg(err.Error())
		return err
	}

	deployment.Spec.Template.Spec.Containers[0].Resources.Requests = v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpuReq, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(memReq, resource.BinarySI),
	}

	deployment.Spec.Template.Spec.Containers[0].Resources.Limits = v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpuLimit, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(memLimit, resource.BinarySI),
	}

	originalDeployment, err := clientset.AppsV1().Deployments(globalns).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		log.Error().Msg(err.Error())
		return err
	}

	// Get the original deployment as raw bytes
	original, err := json.Marshal(originalDeployment)
	if err != nil {
		return err
	}

	// Get the modified deployment as raw bytes
	modified, err := json.Marshal(deployment)
	if err != nil {
		return err
	}

	// Create the patch
	patch, err := strategicpatch.CreateTwoWayMergePatch(original, modified, appsv1.Deployment{})
	if err != nil {
		return err
	}
	if len(patch) <= 2 {
		return nil
	}

	log.Info().Msgf("Patching %s: CPU[req=%d, limit=%d] MEM[req=%d,limit=%d]",
		conf.Agent[index].Name, cpuReq, cpuLimit, memReq, memLimit)
	_, err = clientset.AppsV1().Deployments(globalns).Patch(context.Background(), agentName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		log.Error().Msgf("Error patching pod: %v", err)
		return err
	}
	return err
}

// wait for the deployment to be ready
func deploymentReady(clientset *kubernetes.Clientset, globalns string, factory informers.SharedInformerFactory) {
	deploymentLister := factory.Apps().V1().Deployments().Lister()
	// Loop until all deployments are ready
	for {
		deployments, err := deploymentLister.List(labels.Everything())
		if err != nil {
			log.Error().Msgf("Deployment not found: %v", err)
			return
		}

		allReady := true
		for _, deployment := range deployments {
			if deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
				allReady = false
				break
			}
		}
		if allReady {
			break
		}
		time.Sleep(2 * time.Second)
	}
}

func getEnv(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func installAgents(clientset *kubernetes.Clientset, cfg *action.Configuration, settings *cli.EnvSettings, conf AgentConfig, i int, name, chartRef, ns string) {
	// Get the values of the environment variables.
	tenantID := getEnv("tenant_id", "0")
	clusterID := getEnv("cluster_id", "0")
	clusterName := getEnv("cluster_name", "default")
	workspaceID := getEnv("workspace_id", "0")

	env := fmt.Sprint("serviceAccount.Namespace=", globalns, ",env.tenant_id=", tenantID, ",env.workspace_id=", workspaceID, ",env.cluster_name=", clusterName, ",env.cluster_id=", clusterID)
	var args = map[string]string{
		"setenv": env,
	}

	client := action.NewInstall(cfg)

	// Locate the chart in the chart repository.
	chartPath, err := client.LocateChart(chartRef, settings)
	if err != nil {
		log.Error().Msgf("Error locating chart: %v", err)
		return
	}

	// Create a chart object from the chartPath.
	chart, err := loader.Load(chartPath)
	if err != nil {
		log.Error().Msgf("Error loading chart: %v", err)
		return
	}

	client.Namespace = ns
	client.ReleaseName = name

	p := getter.All(settings)
	valueOpts := &values.Options{}
	vals, err := valueOpts.MergeValues(p)
	if err != nil {
		log.Error().Msgf("Error in Mergevalues: %v", err.Error())
		return
	}

	if err := strvals.ParseInto(args["setenv"], vals); err != nil {
		log.Error().Msgf("failed parsing --set env data: %v", err.Error())
		return
	}
	log.Info().Msgf("Env variables passed to helm: %v", vals)

	_, err = client.Run(chart, vals)
	if err != nil {
		log.Error().Msg(err.Error())
		return
	}
}

var mutex sync.Mutex

func main() {
	// get the local kube config
	os.Setenv("HELM_NAMESPACE", globalns)
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		log.Error().Msg(err.Error())
		return
	}

	settings := cli.New()
	// Set up the Helm action configuration.
	cfg := new(action.Configuration)
	if err := cfg.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), log.Error().Msgf); err != nil {
		log.Error().Msgf("%+v", err)
		return
	}

	// create the clientset
	clientset := kubernetes.NewForConfigOrDie(config)

	configMap, err := clientset.CoreV1().ConfigMaps(globalns).Get(context.TODO(), agentConfig, metav1.GetOptions{})
	if err != nil {
		log.Error().Msgf("Error getting config: %v", err.Error())
		return
	}

	var conf AgentConfig
	err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		log.Error().Msgf("Error parsing config: %v", err.Error())
		return
	}

	// Get the names of all the agents
	for i, agent := range conf.Agent {
		_, err := clientset.AppsV1().Deployments(globalns).Get(context.TODO(), agent.Name, metav1.GetOptions{})
		if err != nil {
			chartRef := agent.ChartName
			log.Info().Msgf("Chartname: %s", chartRef)
			log.Info().Msgf("Agent not found, installing: %s", agent.Name)
			installAgents(clientset, cfg, settings, conf, i, agent.Name, chartRef, globalns)
		}
	}

	nodesCount := 0

	// Node informer

	// Start the informer
	stopCh := make(chan struct{})

	// Create shared informer factory
	factory := informers.NewSharedInformerFactory(clientset, 0)

	dfactory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(globalns))

	// Retrieve the node informer
	nodeInformer := factory.Core().V1().Nodes().Informer()

	// Set up an event handler for when nodes are added or deleted
	_, _ = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			mutex.Lock()
			nodesCount = nodesCount + 1
			mutex.Unlock()
			log.Info().Msgf("add node: nodesCount = %d", nodesCount)
			updateAllAgents(clientset, nodesCount)
		},
		DeleteFunc: func(obj interface{}) {
			mutex.Lock()
			nodesCount = nodesCount - 1
			mutex.Unlock()
			log.Info().Msgf("del node: nodesCount = %d", nodesCount)
			updateAllAgents(clientset, nodesCount)
		},
	})

	// Retrieve the deployment informer
	deploymentInformer := dfactory.Apps().V1().Deployments().Informer()

	// Set up an event handler for when deployments are added or deleted
	_, _ = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// Print the name of the newly added deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				log.Info().Msgf("New deployment detected: %s", deployment.Name)
				deploymentReady(clientset, globalns, dfactory)
				updateAllAgents(clientset, nodesCount)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment, ok := oldObj.(*appsv1.Deployment)
			if !ok {
				return
			}
			newDeployment, ok := newObj.(*appsv1.Deployment)
			if !ok {
				return
			}

			// Check if the deployment has been updated and if it is now ready
			if oldDeployment.ResourceVersion != newDeployment.ResourceVersion && newDeployment.Status.ReadyReplicas == *newDeployment.Spec.Replicas {
				log.Info().Msgf("Deployment updated: %s", newDeployment.Name)
				deploymentReady(clientset, globalns, dfactory)
				updateAllAgents(clientset, nodesCount)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Print the name of the deleted deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				log.Info().Msgf("Deployment deleted: %s", deployment.Name)
			}
		},
	})

	// Retrieve the deployment informer
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()

	// Configmap informer
	_, _ = configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldConfigMap, ok := oldObj.(*v1.ConfigMap)
			if !ok {
				return
			}
			newConfigMap, ok := newObj.(*v1.ConfigMap)
			if !ok {
				return
			}
			if oldConfigMap.ResourceVersion != newConfigMap.ResourceVersion {
				if newConfigMap.Name == agentConfig {
					log.Info().Msgf("Configmap updated: %s", newConfigMap.Name)
					mutex.Lock()
					configMapUpdated = true
					mutex.Unlock()
					updateAllAgents(clientset, nodesCount)
				}
			}
		},
	})

	// Run the node informer with the stop channel
	go nodeInformer.Run(stopCh)
	// Run the deployment informer with the stop channel
	go deploymentInformer.Run(stopCh)
	// Run the configmap informer with the stop channel
	go configMapInformer.Run(stopCh)

	wait.Until(func() {}, time.Second, stopCh)
	// Close the stop channel to signal the informers to stop
	close(stopCh)
}
