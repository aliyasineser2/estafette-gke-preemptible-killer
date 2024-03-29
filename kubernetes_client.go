package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

//go:generate mockgen -package=main -destination ./kubernetes_client_mock.go -source=kubernetes_client.go
type KubernetesClient interface {
	DrainNode(ctx context.Context, nodeName string, drainTimeout int) (err error)
	DrainKubeDNSFromNode(ctx context.Context, nodeName string, drainTimeout int) (err error)
	GetNode(ctx context.Context, nodeName string) (node *v1.Node, err error)
	DeleteNode(ctx context.Context, nodeName string) (err error)
	GetPreemptibleNodes(ctx context.Context, filters map[string]string) (nodes *v1.NodeList, err error)
	GetProjectIdAndZoneFromNode(ctx context.Context, nodeName string) (projectID string, zone string, err error)
	SetNodeAnnotation(ctx context.Context, nodeName string, key string, value string) (err error)
	SetUnschedulableState(ctx context.Context, nodeName string, unschedulable bool) (err error)
}

// NewKubernetesClient return a Kubernetes client
func NewKubernetesClient(kubeClientset *kubernetes.Clientset) (kubernetes KubernetesClient, err error) {
	return &kubernetesClient{
		kubeClientset: kubeClientset,
	}, nil
}

type kubernetesClient struct {
	kubeClientset *kubernetes.Clientset
}

// GetProjectIdAndZoneFromNode returns project id and zone from given node name
// by getting informations from node spec provider id
func (c *kubernetesClient) GetProjectIdAndZoneFromNode(ctx context.Context, nodeName string) (projectID string, zone string, err error) {
	node, err := c.GetNode(ctx, nodeName)

	if err != nil {
		return
	}

	s := strings.Split(node.Spec.ProviderID, "/")
	projectID = s[2]
	zone = s[3]

	return
}

// GetPreemptibleNodes return a list of preemptible node
func (c *kubernetesClient) GetPreemptibleNodes(ctx context.Context, filters map[string]string) (nodes *v1.NodeList, err error) {

	labelSelector := labels.Set{
		"cloud.google.com/gke-preemptible": "true",
	}

	for key, value := range filters {
		labelSelector[key] = value
	}

	nodes, err = c.kubeClientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector.String(),
	})
	if err != nil {
		return
	}

	return
}

// GetNode return the node object from given name
func (c *kubernetesClient) GetNode(ctx context.Context, nodeName string) (node *v1.Node, err error) {
	node, err = c.kubeClientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return
	}

	return
}

func (c *kubernetesClient) DeleteNode(ctx context.Context, nodeName string) (err error) {
	err = c.kubeClientset.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	if err != nil {
		return
	}

	return
}

// SetNodeAnnotation add an annotation (key/value) to a node from a given node name
// As the nodes are constantly being updated, the k8s client doesn't support patch feature yet and
// to reduce the chance to hit a failure 409 we fetch the node before update
func (c *kubernetesClient) SetNodeAnnotation(ctx context.Context, nodeName string, key string, value string) (err error) {
	newNode, err := c.GetNode(ctx, nodeName)

	if err != nil {
		err = fmt.Errorf("Error getting node information before setting annotation:\n%v", err)
		return
	}

	newNode.ObjectMeta.Annotations[key] = value

	_, err = c.kubeClientset.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{})
	if err != nil {
		return
	}

	return
}

// SetUnschedulableState set the unschedulable state of a given node name
func (c *kubernetesClient) SetUnschedulableState(ctx context.Context, nodeName string, unschedulable bool) (err error) {
	node, err := c.GetNode(ctx, nodeName)

	if err != nil {
		err = fmt.Errorf("Error getting node information before setting unschedulable state:\n%v", err)
		return
	}

	node.Spec.Unschedulable = unschedulable

	_, err = c.kubeClientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return
	}

	return
}

// filterOutPodByOwnerReferenceKind filter out a list of pods by its owner references kind
func filterOutPodByOwnerReferenceKind(podList []v1.Pod, kind string) (output []v1.Pod) {
	for _, pod := range podList {
		for _, ownerReference := range pod.ObjectMeta.OwnerReferences {
			if ownerReference.Kind != kind {
				output = append(output, pod)
			}
		}
	}

	return
}

// filterOutPodByNode filters out a list of pods by its node
func filterOutPodByNode(podList []v1.Pod, nodeName string) (output []v1.Pod) {
	for _, pod := range podList {
		if pod.Spec.NodeName == nodeName {
			output = append(output, pod)
		}
	}

	return
}

// DrainNode delete every pods from a given node and wait that all pods are removed before it succeed
// it also make sure we don't select DaemonSet because they are not subject to unschedulable state
func (c *kubernetesClient) DrainNode(ctx context.Context, nodeName string, drainTimeout int) (err error) {
	// Select all pods sitting on the node except the one from kube-system

	fieldSelector := fmt.Sprintf("spec.nodeName=%v,metadata.namespace!=kube-system", nodeName)

	podList, err := c.kubeClientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fieldSelector,
	})

	if err != nil {
		return
	}

	// Filter out DaemonSet from the list of pods
	filteredPodList := filterOutPodByOwnerReferenceKind(podList.Items, "DaemonSet")

	log.Info().
		Str("host", nodeName).
		Msgf("%d pod(s) found", len(filteredPodList))

	stopEvicting := make(chan bool)
	stopPolling := make(chan bool)
	errCh := make(chan error)
	defer func() {
		if len(errCh) > 0 {
			err = <-errCh
		}
	}()

	go func() {
		if err := c.evictPods(ctx, filteredPodList, stopEvicting); err != nil {
			errCh <- err
		}
	}()

	doneDraining := make(chan bool)
	// Wait until all pods are deleted
	go func() {
		for {
			sleepTime := ApplyJitter(10)
			sleepDuration := time.Duration(sleepTime) * time.Second
			pendingPodList, err := c.kubeClientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
				FieldSelector: fieldSelector,
			})

			if err != nil {
				log.Error().
					Err(err).
					Str("host", nodeName).
					Msgf("Error getting list of pods, sleeping %ds", sleepTime)

				time.Sleep(sleepDuration)
				continue
			}

			// Filter out DaemonSet from the list of pods
			filteredPendingPodList := filterOutPodByOwnerReferenceKind(pendingPodList.Items, "DaemonSet")
			podsPending := len(filteredPendingPodList)

			if podsPending == 0 {
				doneDraining <- true
				return
			}

			log.Info().
				Str("host", nodeName).
				Msgf("%d pod(s) pending deletion, sleeping %ds", podsPending, sleepTime)

			select {
			case <-stopPolling:
				return
			default:
				time.Sleep(sleepDuration)
			}
		}
	}()

	select {
	case <-doneDraining:
		break
	case <-time.After(time.Duration(drainTimeout) * time.Second):
		log.Warn().
			Str("host", nodeName).
			Msg("Draining node timeout reached")
		close(stopPolling)
		close(stopEvicting)
		return
	case <-ctx.Done():
		close(stopPolling)
		close(stopEvicting)
		return
	}

	log.Info().
		Str("host", nodeName).
		Msg("Done draining node")

	return
}

// DrainKubeDNSFromNode deletes any kube-dns pods running on the node
func (c *kubernetesClient) DrainKubeDNSFromNode(ctx context.Context, nodeName string, drainTimeout int) (err error) {
	// Select all pods sitting on the node except the one from kube-system
	labelSelector := labels.Set{
		"k8s-app": "kube-dns",
	}

	podList, err := c.kubeClientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector.String(),
	})

	if err != nil {
		return
	}

	// Filter out pods running on other nodes
	filteredPodList := filterOutPodByNode(podList.Items, nodeName)

	log.Info().
		Str("host", nodeName).
		Msgf("%d kube-dns pod(s) found", len(filteredPodList))

	stopEvicting := make(chan bool)
	stopPolling := make(chan bool)
	errCh := make(chan error)
	defer func() {
		if len(errCh) > 0 {
			err = <-errCh
		}
	}()

	go func() {
		if err := c.evictPods(ctx, filteredPodList, stopEvicting); err != nil {
			errCh <- err
		}
	}()

	doneDraining := make(chan bool)
	// Wait until all pods are deleted
	go func() {
		for {
			sleepTime := ApplyJitter(10)
			sleepDuration := time.Duration(sleepTime) * time.Second
			podList, err := c.kubeClientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector.String(),
			})

			if err != nil {
				log.Error().
					Err(err).
					Str("host", nodeName).
					Msgf("Error getting list of kube-dns pods, sleeping %ds", sleepTime)

				time.Sleep(sleepDuration)
				continue
			}

			// Filter out DaemonSet from the list of pods
			filteredPendingPodList := filterOutPodByNode(podList.Items, nodeName)
			podsPending := len(filteredPendingPodList)

			if podsPending == 0 {
				doneDraining <- true
				return
			}

			log.Info().
				Str("host", nodeName).
				Msgf("%d pod(s) pending deletion, sleeping %ds", podsPending, sleepTime)

			select {
			case <-stopPolling:
				return
			default:
				time.Sleep(sleepDuration)
			}
		}
	}()

	select {
	case <-doneDraining:
		break
	case <-time.After(time.Duration(drainTimeout) * time.Second):
		log.Warn().
			Str("host", nodeName).
			Msg("Draining kube-dns node timeout reached")
		close(stopPolling)
		close(stopEvicting)
		return
	case <-ctx.Done():
		close(stopPolling)
		close(stopEvicting)
		return
	}

	log.Info().
		Str("host", nodeName).
		Msg("Done draining kube-dns from node")

	return
}

func (c *kubernetesClient) evictPods(ctx context.Context, pods []v1.Pod, stop <-chan bool) (lastErr error) {
	podsPerBatch := 10
	numPodsLeft := len(pods)
	podsProcessedSoFar := 0
	errCh := make(chan error)
	defer func() {
		if len(errCh) > 0 {
			thisErr := <-errCh
			lastErr = fmt.Errorf("error evicting pods, last error was: %s", thisErr.Error())
		}
	}()

	for numPodsLeft > 0 {
		numPodsThisBatch := int(math.Min(float64(numPodsLeft), float64(podsPerBatch)))
		podsThisBatch := pods[podsProcessedSoFar : podsProcessedSoFar+numPodsThisBatch]
		stopChs := make([]chan bool, numPodsThisBatch)
		for i := 0; i < len(stopChs); i++ {
			stopChs[i] = make(chan bool)
		}
		wg := &sync.WaitGroup{}
		for i := 0; i < numPodsThisBatch; i++ {
			wg.Add(1)
			go func(i int) {
				thisPod := podsThisBatch[i]
				if err := c.evictPod(ctx, thisPod, stopChs[i]); err != nil {
					log.Error().
						Err(err).
						Msgf("failed to evict pod %s", thisPod.Name)
					errCh <- err
				}
				wg.Done()
			}(i)
		}

		select {
		case <-stop:
			for _, ch := range stopChs {
				close(ch)
			}
			return
		default:
			wg.Wait()
			numPodsLeft -= numPodsThisBatch
			podsProcessedSoFar += numPodsThisBatch
		}
	}
	return
}

func (c *kubernetesClient) evictPod(ctx context.Context, pod v1.Pod, stop <-chan bool) error {
	log.Info().
		Str("host", pod.Spec.NodeName).
		Msgf("Evicting pod %s", pod.Name)
	eviction := &v1beta1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	for {
		err := c.kubeClientset.PolicyV1beta1().Evictions(eviction.Namespace).Evict(ctx, eviction)
		if err == nil {
			log.Info().
				Msgf("pod %s evicted", pod.Name)
			break
		} else if errors.IsNotFound(err) {
			log.Info().
				Msgf("pod %s already gone", pod.Name)
			break
		} else if errors.IsTooManyRequests(err) { //We get a 429 in the case of disruption budget related failures
			log.Info().
				Err(err).
				Msgf("too many evictions while evicting %s, this may be due to pod disruption budget. trying again soon", pod.Name)
			time.Sleep(5 * time.Second)
		} else if errors.IsForbidden(err) && errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
			log.Warn().
				Msgf("cannot evict %s, namespace is being deleted", pod.Name)
			//namespace is being deleted, finalizers should take care of deleting the pod
			break
		} else {
			return err
		}
		select {
		case <-stop:
			return nil
		default:
		}
	}
	return nil
}
