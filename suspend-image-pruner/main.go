package main

import (
	_ "embed"
	"log"
	"os/signal"
	"syscall"
	"time"

	"context"
	"fmt"
	"os"
	"strings"

	imageregistryv1 "github.com/openshift/api/imageregistry/v1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"

	cr "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
)

var (
	hiveScheme        = runtime.NewScheme()
	deployementScheme = runtime.NewScheme()
	YamlSerializer    = json.NewSerializerWithOptions(
		json.DefaultMetaFactory, hiveScheme, hiveScheme,
		json.SerializerOptions{Yaml: true, Pretty: true, Strict: true},
	)
)

func init() {
	corev1.AddToScheme(hiveScheme)
	hivev1.AddToScheme(hiveScheme)

	corev1.AddToScheme(deployementScheme)
	imageregistryv1.AddToScheme(deployementScheme)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs
		cancel()
	}()

	// assumes KUBECONFIG is set
	client, err := crclient.New(cr.GetConfigOrDie(), crclient.Options{Scheme: hiveScheme})
	if err != nil {
		log.Fatalf("unable to get client: %v", err)
		os.Exit(1)
	}

	err = fixStuckClusterDeployments(ctx, client)
	if err != nil {
		log.Printf("error looking for stuck deployments: %v", err)
	}

	for {
		select {
		case <-time.After(time.Minute):
			err = fixStuckClusterDeployments(ctx, client)
			if err != nil {
				log.Printf("error looking for stuck deployments: %v", err)
			}
		case <-ctx.Done():
			os.Exit(0)
		}
	}
}

func unstickClusterDeployment(ctx context.Context, client crclient.Client, name string) error {
	var clusterProvisionList hivev1.ClusterProvisionList
	if err := client.List(ctx, &clusterProvisionList, crclient.InNamespace(name)); err != nil {
		return fmt.Errorf("unable to list cluster provisions: %v", err)
	}
	for _, provision := range clusterProvisionList.Items {
		if provision.Spec.Stage != hivev1.ClusterProvisionStageComplete {
			continue
		}
		var kubeconfigSecret corev1.Secret
		secretName := fmt.Sprintf("%s-admin-kubeconfig", provision.Name)
		if err := client.Get(ctx, types.NamespacedName{Namespace: name, Name: secretName}, &kubeconfigSecret); err != nil {
			return fmt.Errorf("unable to get kubeconfig secret: %v", err)
		}
		kubeconfig, ok := kubeconfigSecret.Data["kubeconfig"]
		if !ok {
			return fmt.Errorf("kubeconfig secret does not have kubeconfig data key")
		}
		config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			return fmt.Errorf("unable to create rest config from kubeconfig: %v", err)
		}
		deploymentClient, err := crclient.New(config, crclient.Options{Scheme: deployementScheme})
		if err != nil {
			return fmt.Errorf("unable to create client: %v", err)
		}
		var imagePruner imageregistryv1.ImagePruner
		if err := deploymentClient.Get(ctx, types.NamespacedName{Namespace: "", Name: "cluster"}, &imagePruner); err != nil {
			return fmt.Errorf("unable to get cluster image pruner: %v", err)
		}
		if imagePruner.Spec.Suspend != nil && *imagePruner.Spec.Suspend {
			log.Printf("image pruner already suspended for ClusterDeployment %s", name)
			continue
		}
		patchedImagePruner := imagePruner.DeepCopy()
		suspend := true
		patchedImagePruner.Spec.Suspend = &suspend
		if err := deploymentClient.Patch(ctx, &imagePruner, crclient.MergeFrom(patchedImagePruner)); err != nil {
			return fmt.Errorf("unable to patch cluster image pruner: %v", err)
		}
		log.Printf("image pruner suspended for ClusterDeployment %s", name)
		break
	}
	return nil
}

func fixStuckClusterDeployments(ctx context.Context, client crclient.Client) error {
	log.Print("looking for stuck ClusterDeployments")

	var clusterPoolList hivev1.ClusterPoolList
	err := client.List(ctx, &clusterPoolList, crclient.InNamespace("hypershift-cluster-pool"))
	if err != nil {
		return fmt.Errorf("unable to list cluster pools: %v", err)
	}
	for _, clusterpool := range clusterPoolList.Items {
		var namespaceList corev1.NamespaceList
		err = client.List(ctx, &namespaceList)
		if err != nil {
			return fmt.Errorf("unable to list namespaces: %v", err)
		}
		for _, ns := range namespaceList.Items {
			if !strings.HasPrefix(ns.Name, clusterpool.Name) {
				continue
			}
			var clusterDeploymentList hivev1.ClusterDeploymentList
			err = client.List(ctx, &clusterDeploymentList, crclient.InNamespace(ns.Name))
			if err != nil {
				return fmt.Errorf("unable to list cluster deployments: %v", err)
			}
			clusterDeployment := &clusterDeploymentList.Items[0]
			if clusterDeployment.Status.PowerState == hivev1.ClusterPowerStateWaitingForClusterOperators {
				log.Printf("found ClusterDeployment %s with PowerState WaitingForClusterOperators", clusterDeployment.Name)
				if err := unstickClusterDeployment(ctx, client, clusterDeployment.Name); err != nil {
					return fmt.Errorf("unable to unstick cluster deployment %s: %v", clusterDeployment.Name, err)
				}
			}
		}
	}
	return nil
}
