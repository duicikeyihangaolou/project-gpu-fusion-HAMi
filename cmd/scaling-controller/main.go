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

package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/scaling"
)

func main() {
	klog.InitFlags(nil)

	config, err := loadConfig()
	if err != nil {
		klog.Fatalf("Failed to load kubeconfig: %v", err)
	}

	controller, err := scaling.NewController(config)
	if err != nil {
		klog.Fatalf("Failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		klog.InfoS("Received signal, shutting down", "signal", sig)
		cancel()
	}()

	klog.Info("Starting HAMi VGPUScaling controller")
	if err := controller.Run(ctx); err != nil {
		klog.Fatalf("Controller error: %v", err)
	}
}

func loadConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Infof("BuildConfigFromFlags failed: %v, trying in-cluster config", err)
		return rest.InClusterConfig()
	}
	return config, nil
}
