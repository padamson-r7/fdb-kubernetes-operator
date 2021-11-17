/*
 * pod_client.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2018-2019 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package internal

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	fdbtypes "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta1"
	"github.com/docker/docker/daemon/logger"
	"github.com/hashicorp/go-retryablehttp"
	corev1 "k8s.io/api/core/v1"
)

type FDBImageType string

const (
	// MockUnreachableAnnotation defines if a Pod should be unreachable. This annotation
	// is currently only used for testing cases.
	MockUnreachableAnnotation = "foundationdb.org/mock-unreachable"

	// FDBImageTypeUnified indicates that a pod is using a unified image for the
	// main container and sidecar container.
	FDBImageTypeUnified FDBImageType = "unified"

	// FDBImageTypeSplit indicates that a pod is using a different image for the
	// main container and sidecar container.
	FDBImageTypeSplit FDBImageType = "split"

	// CurrentConfigurationAnnotation is the annotation we use to store the
	// latest configuration.
	CurrentConfigurationAnnotation = "foundationdb.org/launcher-current-configuration"

	// EnvironmentAnnotation is the annotation we use to store the environment
	// variables.
	EnvironmentAnnotation = "foundationdb.org/launcher-environment"
)

// FdbPodClient provides methods for working with a FoundationDB pod.
type FdbPodClient interface {
	// GetCluster returns the cluster associated with a client
	GetCluster() *fdbtypes.FoundationDBCluster

	// GetPod returns the pod associated with a client
	GetPod() *corev1.Pod

	// IsPresent checks whether a file is present.
	IsPresent(path string) (bool, error)

	// UpdateFile checks if a file is up-to-date and tries to update it.
	UpdateFile(name string, contents string) (bool, error)

	// GetVariableSubstitutions gets the current keys and values that this
	// process group will substitute into its monitor conf.
	GetVariableSubstitutions() (map[string]string, error)
}

// realPodSidecarClient provides a client for use in real environments, using
// the Kubernetes sidecar.
type realFdbPodSidecarClient struct {
	// Cluster is the cluster we are connecting to.
	Cluster *fdbtypes.FoundationDBCluster

	// Pod is the pod we are connecting to.
	Pod *corev1.Pod

	// useTLS indicates whether this is using a TLS connection to the sidecar.
	useTLS bool

	// tlsConfig contains the TLS configuration for the connection to the
	// sidecar.
	tlsConfig *tls.Config
}

// realPodSidecarClient provides a client for use in real environments, using
// the annotations from the unified Kubernetes image.
type realFdbPodAnnotationClient struct {
	// Cluster is the cluster we are connecting to.
	Cluster *fdbtypes.FoundationDBCluster

	// Pod is the pod we are connecting to.
	Pod *corev1.Pod
}

// NewFdbPodClient builds a client for working with an FDB Pod
func NewFdbPodClient(cluster *fdbtypes.FoundationDBCluster, pod *corev1.Pod) (FdbPodClient, error) {
	if getImageType(pod) == FDBImageTypeUnified {
		return &realFdbPodAnnotationClient{Cluster: cluster, Pod: pod}, nil
	}

	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("waiting for pod %s/%s/%s to be assigned an IP", cluster.Namespace, cluster.Name, pod.Name)
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == "foundationdb-kubernetes-sidecar" && !container.Ready {
			return nil, fmt.Errorf("waiting for pod %s/%s/%s to be ready", cluster.Namespace, cluster.Name, pod.Name)
		}
	}

	useTLS := podHasSidecarTLS(pod)

	var tlsConfig = &tls.Config{}
	if useTLS {
		certFile := os.Getenv("FDB_TLS_CERTIFICATE_FILE")
		keyFile := os.Getenv("FDB_TLS_KEY_FILE")
		caFile := os.Getenv("FDB_TLS_CA_FILE")

		if certFile == "" || keyFile == "" || caFile == "" {
			return nil, errors.New("missing one or more TLS env vars: FDB_TLS_CERTIFICATE_FILE, FDB_TLS_KEY_FILE or FDB_TLS_CA_FILE")
		}

		cert, err := tls.LoadX509KeyPair(
			certFile,
			keyFile,
		)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		if os.Getenv("DISABLE_SIDECAR_TLS_CHECK") == "1" {
			tlsConfig.InsecureSkipVerify = true
		}
		certPool := x509.NewCertPool()
		caList, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		certPool.AppendCertsFromPEM(caList)
		tlsConfig.RootCAs = certPool
	}

	return &realFdbPodSidecarClient{Cluster: cluster, Pod: pod, useTLS: useTLS, tlsConfig: tlsConfig}, nil
}

// GetCluster returns the cluster associated with a client
func (client *realFdbPodSidecarClient) GetCluster() *fdbtypes.FoundationDBCluster {
	return client.Cluster
}

// GetPod returns the pod associated with a client
func (client *realFdbPodSidecarClient) GetPod() *corev1.Pod {
	return client.Pod
}

// getListenIP gets the IP address that a pod listens on.
func (client *realFdbPodSidecarClient) getListenIP() string {
	ips := GetPublicIPsForPod(client.Pod)
	if len(ips) > 0 {
		return ips[0]
	}

	return ""
}

// makeRequest submits a request to the sidecar.
func (client *realFdbPodSidecarClient) makeRequest(method string, path string) (string, error) {
	var resp *http.Response
	var err error

	protocol := "http"
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 2
	retryClient.RetryWaitMax = 1 * time.Second
	// Prevent logging
	retryClient.Logger = nil
	retryClient.CheckRetry = retryablehttp.ErrorPropagatedRetryPolicy

	if client.useTLS {
		retryClient.HTTPClient.Transport = &http.Transport{TLSClientConfig: client.tlsConfig}
		protocol = "https"
	}

	url := fmt.Sprintf("%s://%s:8080/%s", protocol, client.getListenIP(), path)
	switch method {
	case http.MethodGet:
		// We assume that a get request should be relative fast.
		retryClient.HTTPClient.Timeout = 5 * time.Second
		resp, err = retryClient.Get(url)
	case http.MethodPost:
		// A post request could take a little bit longer since we copy sometimes files.
		retryClient.HTTPClient.Timeout = 10 * time.Second
		resp, err = retryClient.Post(url, "application/json", strings.NewReader(""))
	default:
		return "", fmt.Errorf("unknown HTTP method %s", method)
	}

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	bodyText := string(body)

	if err != nil {
		return "", err
	}

	return bodyText, nil
}

// IsPresent checks whether a file in the sidecar is present.
func (client *realFdbPodSidecarClient) IsPresent(filename string) (bool, error) {
	_, err := client.makeRequest("GET", fmt.Sprintf("check_hash/%s", filename))
	if err != nil {
		log.Info("Waiting for file",
			"namespace", client.GetCluster().Namespace,
			"cluster", client.GetCluster().Name,
			"pod", client.GetPod().Name,
			"file", filename)

		return false, err
	}

	return true, nil
}

// CheckHash checks whether a file in the sidecar has the expected contents.
func (client *realFdbPodSidecarClient) checkHash(filename string, contents string) (bool, error) {
	response, err := client.makeRequest("GET", fmt.Sprintf("check_hash/%s", filename))
	if err != nil {
		return false, err
	}

	expectedHash := sha256.Sum256([]byte(contents))
	expectedHashString := hex.EncodeToString(expectedHash[:])
	return strings.Compare(expectedHashString, response) == 0, nil
}

// GenerateMonitorConf updates the monitor conf file for a pod
func (client *realFdbPodSidecarClient) generateMonitorConf() error {
	_, err := client.makeRequest("POST", "copy_monitor_conf")
	return err
}

// copyFiles copies the files from the config map to the shared dynamic conf
// volume
func (client *realFdbPodSidecarClient) copyFiles() error {
	_, err := client.makeRequest("POST", "copy_files")
	return err
}

// GetVariableSubstitutions gets the current keys and values that this
// process group will substitute into its monitor conf.
func (client *realFdbPodSidecarClient) GetVariableSubstitutions() (map[string]string, error) {
	contents, err := client.makeRequest("GET", "substitutions")
	if err != nil {
		return nil, err
	}
	substitutions := map[string]string{}
	err = json.Unmarshal([]byte(contents), &substitutions)
	if err != nil {
		log.Error(err, "Error deserializing pod substitutions", "responseBody", contents)
	}
	return substitutions, err
}

// UpdateFile checks if a file is up-to-date and tries to update it.
func (client *realFdbPodSidecarClient) UpdateFile(name string, contents string) (bool, error) {
	if name == "fdbmonitor.conf" {
		return client.updateDynamicFiles(name, contents, func(client *realFdbPodSidecarClient) error { return client.generateMonitorConf() })
	}
	return client.updateDynamicFiles(name, contents, func(client *realFdbPodSidecarClient) error { return client.copyFiles() })
}

// updateDynamicFiles checks if the files in the dynamic conf volume match the
// expected contents, and tries to copy the latest files from the input volume
// if they do not.
func (client *realFdbPodSidecarClient) updateDynamicFiles(filename string, contents string, updateFunc func(client *realFdbPodSidecarClient) error) (bool, error) {
	match := false
	var err error

	match, err = client.checkHash(filename, contents)
	if err != nil {
		return false, err
	}

	if !match {
		err = updateFunc(client)
		if err != nil {
			return false, err
		}
		// We check this more or less instantly, maybe we should add some delay?
		match, err = client.checkHash(filename, contents)
		if !match {
			logger.Info("Waiting for config update", "file", filename)
		}

		return match, err
	}

	return true, nil
}

// GetCluster returns the cluster associated with a client
func (client *realFdbPodAnnotationClient) GetCluster() *fdbtypes.FoundationDBCluster {
	return client.Cluster
}

// GetPod returns the pod associated with a client
func (client *realFdbPodAnnotationClient) GetPod() *corev1.Pod {
	return client.Pod
}

// GetVariableSubstitutions gets the current keys and values that this
// instance will substitute into its monitor conf.
func (client *realFdbPodAnnotationClient) GetVariableSubstitutions() (map[string]string, error) {
	environmentData, present := client.Pod.Annotations[EnvironmentAnnotation]
	if !present {
		log.Info("Waiting for Kubernetes monitor to update annotations",
			"namespace", client.GetCluster().Namespace,
			"cluster", client.GetCluster().Name,
			"pod", client.GetPod().Name,
			"annotation", EnvironmentAnnotation)
		return nil, fdbPodAnnotationErrorMissingAnnotations
	}
	environment := make(map[string]string)
	err := json.Unmarshal([]byte(environmentData), &environment)
	if err != nil {
		return nil, err
	}

	return environment, nil
}

// UpdateFile checks if a file is up-to-date and tries to update it.
func (client *realFdbPodAnnotationClient) UpdateFile(name string, contents string) (bool, error) {
	if name == "fdb.cluster" {
		// We can ignore cluster file updates in the unified image.
		return true, nil
	}
	if name == "fdbmonitor.conf" {
		desiredConfiguration := KubernetesMonitorProcessConfiguration{}
		err := json.Unmarshal([]byte(contents), &desiredConfiguration)
		if err != nil {
			log.Error(err, "Error parsing desired process configuration", "input", contents)
			return false, err
		}
		currentConfiguration := KubernetesMonitorProcessConfiguration{}
		currentData, present := client.Pod.Annotations[CurrentConfigurationAnnotation]
		if !present {
			log.Info("Waiting for Kubernetes monitor to update annotations",
				"namespace", client.GetCluster().Namespace,
				"cluster", client.GetCluster().Name,
				"pod", client.GetPod().Name,
				"annotation", currentConfiguration)
			return false, fdbPodAnnotationErrorMissingAnnotations
		}
		err = json.Unmarshal([]byte(currentData), &currentConfiguration)
		if err != nil {
			log.Error(err, "Error parsing current process configuration", "input", currentData)
			return false, err
		}
		match := reflect.DeepEqual(currentConfiguration, desiredConfiguration)
		if !match {
			log.Info("Waiting for Kubernetes monitor config update",
				"namespace", client.GetCluster().Namespace,
				"cluster", client.GetCluster().Name,
				"pod", client.GetPod().Name,
				"desired", desiredConfiguration, "current", currentConfiguration)
		}
		return match, nil
	}
	return false, fmt.Errorf("Unknown file %s", name)
}

// IsPresent checks whether a file in the sidecar is present.
// This implementation always returns true, because the unified image handles
// these checks internally.
func (client *realFdbPodAnnotationClient) IsPresent(filename string) (bool, error) {
	return true, nil
}

// MockFdbPodClient provides a mock connection to a pod
type mockFdbPodClient struct {
	Cluster *fdbtypes.FoundationDBCluster
	Pod     *corev1.Pod
}

// NewMockFdbPodClient builds a mock client for working with an FDB pod
func NewMockFdbPodClient(cluster *fdbtypes.FoundationDBCluster, pod *corev1.Pod) (FdbPodClient, error) {
	return &mockFdbPodClient{Cluster: cluster, Pod: pod}, nil
}

// GetCluster returns the cluster associated with a client
func (client *mockFdbPodClient) GetCluster() *fdbtypes.FoundationDBCluster {
	return client.Cluster
}

// GetPod returns the pod associated with a client
func (client *mockFdbPodClient) GetPod() *corev1.Pod {
	return client.Pod
}

func (client *mockFdbPodClient) UpdateFile(name string, contents string) (bool, error) {
	return true, nil
}

// IsPresent checks whether a file in the sidecar is present.
func (client *mockFdbPodClient) IsPresent(filename string) (bool, error) {
	return true, nil
}

// GetVariableSubstitutions gets the current keys and values that this
// process group will substitute into its monitor conf.
func (client *mockFdbPodClient) GetVariableSubstitutions() (map[string]string, error) {
	substitutions := map[string]string{}

	if client.Pod.Annotations != nil {
		if _, ok := client.Pod.Annotations[MockUnreachableAnnotation]; ok {
			return substitutions, &net.OpError{Op: "mock", Err: fmt.Errorf("not reachable")}
		}
	}

	ipString := GetPublicIPsForPod(client.Pod)[0]
	substitutions["FDB_PUBLIC_IP"] = ipString
	if ipString != "" {
		ip := net.ParseIP(ipString)
		if ip == nil {
			return nil, fmt.Errorf("failed to parse IP from pod: %s", ipString)
		}

		if ip.To4() == nil {
			substitutions["FDB_PUBLIC_IP"] = fmt.Sprintf("[%s]", ipString)
		}
	}

	if client.Cluster.Spec.FaultDomain.Key == "foundationdb.org/none" {
		substitutions["FDB_MACHINE_ID"] = client.Pod.Name
		substitutions["FDB_ZONE_ID"] = client.Pod.Name
	} else if client.Cluster.Spec.FaultDomain.Key == "foundationdb.org/kubernetes-cluster" {
		substitutions["FDB_MACHINE_ID"] = client.Pod.Spec.NodeName
		substitutions["FDB_ZONE_ID"] = client.Cluster.Spec.FaultDomain.Value
	} else {
		faultDomainSource := client.Cluster.Spec.FaultDomain.ValueFrom
		if faultDomainSource == "" {
			faultDomainSource = "spec.nodeName"
		}
		substitutions["FDB_MACHINE_ID"] = client.Pod.Spec.NodeName

		if faultDomainSource == "spec.nodeName" {
			substitutions["FDB_ZONE_ID"] = client.Pod.Spec.NodeName
		} else {
			return nil, fmt.Errorf("unsupported fault domain source %s", faultDomainSource)
		}
	}

	substitutions["FDB_INSTANCE_ID"] = GetProcessGroupIDFromMeta(client.Cluster, client.Pod.ObjectMeta)

	version, err := fdbtypes.ParseFdbVersion(client.Cluster.Spec.Version)
	if err != nil {
		return nil, err
	}

	if version.SupportsUsingBinariesFromMainContainer() {
		if client.Cluster.IsBeingUpgraded() {
			substitutions["BINARY_DIR"] = fmt.Sprintf("/var/dynamic-conf/bin/%s", client.Cluster.Spec.Version)
		} else {
			substitutions["BINARY_DIR"] = "/usr/bin"
		}
	}

	return substitutions, nil
}

// podHasSidecarTLS determines whether a pod currently has TLS enabled for the
// sidecar process.
func podHasSidecarTLS(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == "foundationdb-kubernetes-sidecar" {
			for _, arg := range container.Args {
				if arg == "--tls" {
					return true
				}
			}
		}
	}

	return false
}

// getImageType determines whether a pod is using the unified or the split
// image.
func getImageType(pod *corev1.Pod) FDBImageType {
	for _, container := range pod.Spec.Containers {
		if container.Name != "foundationdb" {
			continue
		}
		for _, envVar := range container.Env {
			if envVar.Name == "FDB_IMAGE_TYPE" {
				return FDBImageType(envVar.Value)
			}
		}
	}
	return FDBImageTypeSplit
}

// fdbPodAnnotationError Describes custom errors returned when getting info from
// pod annotations.
type fdbPodAnnotationError string

const (
	// fdbPodAnnotationErrorMissingAnnotations is returned when a pod is missing
	// a required annotation.
	fdbPodAnnotationErrorMissingAnnotations fdbPodAnnotationError = "MissingAnnotations"
)

func (err fdbPodAnnotationError) Error() string {
	if err == fdbPodAnnotationErrorMissingAnnotations {
		return "Pod does not have required annotation"
	}
	return string(err)
}
