package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	monclientv1 "github.com/coreos/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	machineapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	machineconfigapi "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	muocfg "github.com/openshift/managed-upgrade-operator/config"
	"github.com/openshift/managed-upgrade-operator/pkg/apis"
	"github.com/openshift/managed-upgrade-operator/pkg/controller"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics/collector"
	"github.com/openshift/managed-upgrade-operator/pkg/upgradeconfigmanager"
	"github.com/openshift/managed-upgrade-operator/util"
	"github.com/openshift/managed-upgrade-operator/version"
	opmetrics "github.com/openshift/operator-custom-metrics/pkg/metrics"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	"github.com/operator-framework/operator-sdk/pkg/log/zap"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	sdkVersion "github.com/operator-framework/operator-sdk/version"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost             = "0.0.0.0"
	metricsPort       int32 = 8383
	customMetricsPath       = "/metrics"
)
var log = logf.Log.WithName("cmd")

func printVersion() {
	log.Info(fmt.Sprintf("Operator Version: %s", version.Version))
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
	log.Info(fmt.Sprintf("Version of operator-sdk: %v", sdkVersion.Version))
}

func main() {
	// Add the zap logger flag set to the CLI. The flag set must
	// be added before calling pflag.Parse().
	pflag.CommandLine.AddFlagSet(zap.FlagSet())

	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Parse()

	// Use a zap logr.Logger implementation. If none of the zap
	// flags are configured (or if the zap flag set is not being
	// used), this defaults to a production zap logger.
	//
	// The logger instantiated here can be changed to any logger
	// implementing the logr.Logger interface. This logger will
	// be propagated through the whole operator, generating
	// uniform and structured logs.
	logf.SetLogger(zap.Logger())

	printVersion()

	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Error(err, "Failed to get watch namespace")
		os.Exit(1)
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	ctx := context.TODO()
	// Become the leader before proceeding
	err = leader.Become(ctx, "managed-upgrade-operator-lock")
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// This set the sync period to 5m
	syncPeriod := time.Duration(muocfg.SyncPeriodDefault)

	// Set default manager options
	options := manager.Options{
		Namespace:          namespace,
		MetricsBindAddress: fmt.Sprintf("%s:%d", metricsHost, metricsPort),
		SyncPeriod:         &syncPeriod,
	}

	// Add support for MultiNamespace set in WATCH_NAMESPACE (e.g ns1,ns2)
	// Note that this is not intended to be used for excluding namespaces, this is better done via a Predicate
	// Also note that you may face performance issues when using this with a high number of namespaces.
	// More Info: https://godoc.org/github.com/kubernetes-sigs/controller-runtime/pkg/cache#MultiNamespacedCacheBuilder
	if strings.Contains(namespace, ",") {
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(namespace, ","))
	}

	// Create a new manager to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	log.Info("Registering Components.")

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	if err = configv1.Install(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	if err = routev1.Install(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	if err = machineapi.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}
	if err = machineconfigapi.Install(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	if err := monitoringv1.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "error registering prometheus monitoring objects")
		os.Exit(1)
	}

	// Setup all Controllers
	if err := controller.AddToManager(mgr); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Add the Metrics Service
	if err := addMetrics(ctx, cfg); err != nil {
		log.Error(err, "Metrics service is not added.")
		os.Exit(1)
	}

	// Add the custom Metrics Service
	metricsClient, err := client.New(cfg, client.Options{})
	if err != nil {
		log.Error(err, "unable to create k8s client for upgrade metrics")
		os.Exit(1)
	}

	uCollector, err := collector.NewUpgradeCollector(metricsClient)
	if err != nil {
		log.Error(err, "unable to create upgrade metrics collector")
		os.Exit(1)
	}

	ns, err := util.GetOperatorNamespace()
	if err != nil {
		os.Exit(1)
	}

	customMetrics := opmetrics.NewBuilder(ns, "managed-upgrade-operator-custom-metrics").
		WithPath(customMetricsPath).
		WithCollector(uCollector).
		WithServiceMonitor().
		WithServiceLabel(map[string]string{"name": muocfg.OperatorName}).
		GetConfig()

	if err = opmetrics.ConfigureMetrics(context.TODO(), *customMetrics); err != nil {
		log.Error(err, "Failed to configure custom metrics")
		os.Exit(1)
	}

	// Define stopCh which we'll use to notify the upgradeConfigManager (and any other routine)
	// to stop work. This channel can also be used to signal routines to complete any cleanup
	// work
	stopCh := signals.SetupSignalHandler()

	upgradeConfigManagerClient, err := client.New(cfg, client.Options{})
	if err != nil {
		log.Error(err, "unable to create configmanager client")
		os.Exit(1)
	}

	ucMgr, err := upgradeconfigmanager.NewBuilder().NewManager(upgradeConfigManagerClient)
	if err != nil {
		log.Error(err, "can't read config manager configuration")
	}
	log.Info("Starting UpgradeConfig manager")
	go ucMgr.StartSync(stopCh)

	// Watch the Cmd
	if err := mgr.Start(stopCh); err != nil {
		log.Error(err, "Manager exited non-zero")
		os.Exit(1)
	}
}

// addMetrics will create the Services and Service Monitors to allow the operator export the metrics by using
// the Prometheus operator
func addMetrics(ctx context.Context, cfg *rest.Config) error {
	// Get the namespace the operator is currently deployed in.
	operatorNs, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		if errors.Is(err, k8sutil.ErrRunLocal) {
			log.Info("Skipping CR metrics server creation; not running in a cluster.")
			return nil
		}
	}

	// Add to the below struct any other metrics ports you want to expose.
	servicePorts := []v1.ServicePort{
		{Port: metricsPort, Name: metrics.OperatorPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: metricsPort}},
	}

	// Create Service object to expose the metrics port(s).
	service, err := metrics.CreateMetricsService(ctx, cfg, servicePorts)
	if err != nil {
		log.Info("Could not create metrics Service", "error", err.Error())
	}

	// CreateServiceMonitors will automatically create the prometheus-operator ServiceMonitor resources
	// necessary to configure Prometheus to scrape metrics from this operator.
	services := []*v1.Service{service}

	var serviceMonitors []*monitoringv1.ServiceMonitor
	mclient := monclientv1.NewForConfigOrDie(cfg)

	for _, s := range services {
		if s == nil {
			continue
		}
		sm := GenerateServiceMonitor(s)

		// ErrSMMetricsExists is used to detect if the -metrics ServiceMonitor already exists
		var ErrSMMetricsExists = fmt.Sprintf("servicemonitors.monitoring.coreos.com \"%s-metrics\" already exists", muocfg.OperatorName)

		log.Info(fmt.Sprintf("Attempting to create service monitor %s", sm.Name))
		// TODO: Get SM and compare to see if an UPDATE is required
		smc, err := mclient.ServiceMonitors(operatorNs).Create(sm)
		if err != nil {
			if err.Error() != ErrSMMetricsExists {
				return err
			}
			log.Info("ServiceMonitor already exists")
		}
		log.Info(fmt.Sprintf("Successfully created service monitor %s", sm.Name))
		serviceMonitors = append(serviceMonitors, smc)
	}
	return nil
}

// GenerateServiceMonitor generates a prometheus-operator ServiceMonitor object
// based on the passed Service object.
func GenerateServiceMonitor(s *v1.Service) *monitoringv1.ServiceMonitor {
	labels := make(map[string]string)
	for k, v := range s.ObjectMeta.Labels {
		labels[k] = v
	}
	endpoints := populateEndpointsFromServicePorts(s)
	boolTrue := true

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.ObjectMeta.Name,
			Namespace: s.ObjectMeta.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					BlockOwnerDeletion: &boolTrue,
					Controller:         &boolTrue,
					Kind:               "Service",
					Name:               s.Name,
					UID:                s.UID,
				},
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: labels,
			},
			Endpoints: endpoints,
		},
	}
}

func populateEndpointsFromServicePorts(s *v1.Service) []monitoringv1.Endpoint {
	var endpoints []monitoringv1.Endpoint
	for _, port := range s.Spec.Ports {
		endpoints = append(endpoints, monitoringv1.Endpoint{Port: port.Name})
	}
	return endpoints
}
