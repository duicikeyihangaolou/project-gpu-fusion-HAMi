/*
Copyright 2024 The HAMi Authors.

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

package scaling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/api/v1alpha1"
	"github.com/Project-HAMi/HAMi/pkg/device"
)

var vgpuScalingGVR = schema.GroupVersionResource{
	Group:    v1alpha1.GroupName,
	Version:  v1alpha1.Version,
	Resource: v1alpha1.Resource,
}

type Controller struct {
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
	queue         workqueue.TypedRateLimitingInterface[string]
	informer      cache.SharedIndexInformer
}

func NewController(config *rest.Config) (*Controller, error) {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 30*time.Second)
	informer := factory.ForResource(vgpuScalingGVR).Informer()

	c := &Controller{
		kubeClient:    kubeClient,
		dynamicClient: dynClient,
		restConfig:    config,
		queue:         workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		informer:      informer,
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})

	return c, nil
}

func (c *Controller) Run(ctx context.Context) error {
	defer c.queue.ShutDown()
	klog.Info("Starting VGPUScaling controller")

	go c.informer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	klog.Info("VGPUScaling controller synced and ready")

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			if !c.processNextItem(ctx) {
				return nil
			}
		}
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.reconcile(ctx, key)
	if err != nil {
		klog.ErrorS(err, "Reconcile failed", "key", key)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	obj, err := c.dynamicClient.Resource(vgpuScalingGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if phase == v1alpha1.PhaseCompleted || phase == v1alpha1.PhaseFailed {
		return nil
	}

	spec, err := extractSpec(obj)
	if err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("invalid spec: %v", err), "", 0)
	}

	pod, err := c.kubeClient.CoreV1().Pods(namespace).Get(ctx, spec.TargetPod, metav1.GetOptions{})
	if err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("target pod not found: %v", err), "", 0)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("target pod is not running (phase: %s)", pod.Status.Phase), "", 0)
	}

	containerIdx := -1
	for i, ctr := range pod.Spec.Containers {
		if ctr.Name == spec.TargetContainer {
			containerIdx = i
			break
		}
	}
	if containerIdx < 0 {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("container %q not found in pod", spec.TargetContainer), "", 0)
	}

	annoKey := "hami.io/vgpu-devices-allocated"
	annoVal, ok := pod.Annotations[annoKey]
	if !ok || annoVal == "" {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, "pod has no GPU allocation annotation", "", 0)
	}

	newMemBytes, err := parseMemoryLimit(spec.GPU.MemoryLimit)
	if err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("invalid memoryLimit: %v", err), "", 0)
	}
	newMemMB := int32(newMemBytes / (1024 * 1024))
	newCore := spec.GPU.CoreLimit

	prevMem, prevCore, err := getCurrentLimits(annoVal, containerIdx)
	if err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("parse annotation: %v", err), "", 0)
	}

	if err := c.updateStatus(ctx, obj, v1alpha1.PhaseApplying, "Applying new GPU limits", fmt.Sprintf("%dm", prevMem), prevCore); err != nil {
		return err
	}

	configContent := buildDynamicConfig(newMemMB, newCore)
	if err := c.execWriteConfig(ctx, pod, spec.TargetContainer, configContent); err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("write dynamic config: %v", err), fmt.Sprintf("%dm", prevMem), prevCore)
	}
	klog.InfoS("Dynamic config written", "pod", klog.KObj(pod), "container", spec.TargetContainer, "memMB", newMemMB, "core", newCore)

	newAnno := updateAnnotation(annoVal, containerIdx, newMemMB, newCore)
	patchData, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				annoKey: newAnno,
			},
		},
	})
	_, err = c.kubeClient.CoreV1().Pods(namespace).Patch(ctx, pod.Name, "application/merge-patch+json", patchData, metav1.PatchOptions{})
	if err != nil {
		return c.updateStatus(ctx, obj, v1alpha1.PhaseFailed, fmt.Sprintf("update pod annotation: %v", err), fmt.Sprintf("%dm", prevMem), prevCore)
	}
	klog.InfoS("Pod annotation updated", "pod", klog.KObj(pod), "annotation", newAnno)

	return c.updateStatus(ctx, obj, v1alpha1.PhaseCompleted, "GPU resources scaled successfully", fmt.Sprintf("%dm", prevMem), prevCore)
}

func extractSpec(obj *unstructured.Unstructured) (v1alpha1.VGPUScalingSpec, error) {
	var spec v1alpha1.VGPUScalingSpec
	specMap, ok, _ := unstructured.NestedMap(obj.Object, "spec")
	if !ok {
		return spec, fmt.Errorf("spec not found")
	}
	data, _ := json.Marshal(specMap)
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, err
	}
	if spec.TargetPod == "" || spec.TargetContainer == "" {
		return spec, fmt.Errorf("targetPod and targetContainer are required")
	}
	if spec.GPU.CoreLimit < 0 || spec.GPU.CoreLimit > 100 {
		return spec, fmt.Errorf("coreLimit must be 0-100")
	}
	if spec.GPU.MemoryLimit == "" {
		return spec, fmt.Errorf("memoryLimit is required")
	}
	return spec, nil
}

func parseMemoryLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0, fmt.Errorf("empty memory limit")
	}
	var multiplier int64 = 1
	last := s[len(s)-1]
	switch {
	case last == 'g' || last == 'G':
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case last == 'm' || last == 'M':
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case last == 'k' || last == 'K':
		multiplier = 1024
		s = s[:len(s)-1]
	}
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return val * multiplier, nil
}

func getCurrentLimits(annoVal string, containerIdx int) (int32, int32, error) {
	containers := strings.Split(annoVal, device.OnePodMultiContainerSplitSymbol)
	realIdx := 0
	for _, ctrStr := range containers {
		ctrStr = strings.TrimSpace(ctrStr)
		if ctrStr == "" {
			continue
		}
		if realIdx == containerIdx {
			devs := strings.Split(ctrStr, device.OneContainerMultiDeviceSplitSymbol)
			for _, d := range devs {
				if !strings.Contains(d, ",") {
					continue
				}
				parts := strings.Split(d, ",")
				if len(parts) < 4 {
					continue
				}
				mem, _ := strconv.ParseInt(parts[2], 10, 32)
				core, _ := strconv.ParseInt(parts[3], 10, 32)
				return int32(mem), int32(core), nil
			}
		}
		realIdx++
	}
	return 0, 0, fmt.Errorf("container index %d not found in annotation", containerIdx)
}

func updateAnnotation(annoVal string, containerIdx int, newMem, newCore int32) string {
	containers := strings.Split(annoVal, device.OnePodMultiContainerSplitSymbol)
	var result []string
	realIdx := 0
	for _, ctrStr := range containers {
		if strings.TrimSpace(ctrStr) == "" {
			result = append(result, "")
			continue
		}
		if realIdx == containerIdx {
			devs := strings.Split(ctrStr, device.OneContainerMultiDeviceSplitSymbol)
			var updatedDevs []string
			for _, d := range devs {
				if !strings.Contains(d, ",") {
					updatedDevs = append(updatedDevs, d)
					continue
				}
				parts := strings.Split(d, ",")
				if len(parts) < 4 {
					updatedDevs = append(updatedDevs, d)
					continue
				}
				parts[2] = strconv.Itoa(int(newMem))
				parts[3] = strconv.Itoa(int(newCore))
				updatedDevs = append(updatedDevs, strings.Join(parts, ","))
			}
			result = append(result, strings.Join(updatedDevs, device.OneContainerMultiDeviceSplitSymbol))
		} else {
			result = append(result, ctrStr)
		}
		realIdx++
	}
	return strings.Join(result, device.OnePodMultiContainerSplitSymbol)
}

func buildDynamicConfig(memMB, core int32) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_0=%dm\n", memMB))
	sb.WriteString(fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_0=%d\n", core))
	return sb.String()
}

func (c *Controller) execWriteConfig(ctx context.Context, pod *corev1.Pod, container, content string) error {
	escapedContent := strings.ReplaceAll(content, "'", "'\\''")
	cmd := []string{"sh", "-c", fmt.Sprintf(
		`path="${HAMI_DYNAMIC_CONFIG_PATH:-/usr/local/vgpu/dynamic-config}"; mkdir -p "$(dirname "$path")"; printf '%%s' '%s' > "$path"`,
		escapedContent)}

	req := c.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("exec failed (stderr: %s): %w", stderr.String(), err)
	}
	return nil
}

func (c *Controller) updateStatus(ctx context.Context, obj *unstructured.Unstructured, phase, message, prevMem string, prevCore int32) error {
	status := map[string]interface{}{
		"phase":   phase,
		"message": message,
	}
	if prevMem != "" {
		status["previousMemoryLimit"] = prevMem
		status["previousCoreLimit"] = int64(prevCore)
	}
	if phase == v1alpha1.PhaseCompleted {
		status["appliedAt"] = time.Now().UTC().Format(time.RFC3339)
	}

	patch := map[string]interface{}{
		"status": status,
	}
	patchData, _ := json.Marshal(patch)

	_, err := c.dynamicClient.Resource(vgpuScalingGVR).
		Namespace(obj.GetNamespace()).
		Patch(ctx, obj.GetName(), "application/merge-patch+json", patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.ErrorS(err, "Failed to update VGPUScaling status", "name", obj.GetName(), "phase", phase)
		return err
	}
	klog.InfoS("VGPUScaling status updated", "name", obj.GetName(), "phase", phase, "message", message)
	return nil
}
