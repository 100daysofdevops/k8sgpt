/*
Copyright 2023 The K8sGPT Authors.
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

package analyzer

import (
	"context"
	"fmt"
	"strings"

	"github.com/k8sgpt-ai/k8sgpt/pkg/common"
	"github.com/k8sgpt-ai/k8sgpt/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PodAnalyzer struct {
}

func (PodAnalyzer) Analyze(a common.Analyzer) ([]common.Result, error) {

	kind := "Pod"

	AnalyzerErrorsMetric.DeletePartialMatch(map[string]string{
		"analyzer_name": kind,
	})

	// search all namespaces for pods that are not running
	list, err := a.Client.GetClient().CoreV1().Pods(a.Namespace).List(a.Context, metav1.ListOptions{
		LabelSelector: a.LabelSelector,
	})
	if err != nil {
		return nil, err
	}
	var preAnalysis = map[string]common.PreAnalysis{}

	for _, pod := range list.Items {
		var failures []common.Failure

		// Check for pending pods
		if pod.Status.Phase == "Pending" {
			// Check through container status to check for crashes
			for _, containerStatus := range pod.Status.Conditions {
				if containerStatus.Type == v1.PodScheduled && containerStatus.Reason == "Unschedulable" {
					if containerStatus.Message != "" {
						failures = append(failures, common.Failure{
							Text:      containerStatus.Message,
							Sensitive: []common.Sensitive{},
						})
					}
				}
			}
		}

		// Check for errors in the init containers.
		failures = append(failures, analyzeContainerStatusFailures(a, pod.Status.InitContainerStatuses, pod.Name, pod.Namespace, string(pod.Status.Phase))...)

		// Check for errors in containers.
		failures = append(failures, analyzeContainerStatusFailures(a, pod.Status.ContainerStatuses, pod.Name, pod.Namespace, string(pod.Status.Phase))...)

		if len(failures) > 0 {
			preAnalysis[fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)] = common.PreAnalysis{
				Pod:            pod,
				FailureDetails: failures,
			}
			AnalyzerErrorsMetric.WithLabelValues(kind, pod.Name, pod.Namespace).Set(float64(len(failures)))
		}
	}

	for key, value := range preAnalysis {
		var currentAnalysis = common.Result{
			Kind:  kind,
			Name:  key,
			Error: value.FailureDetails,
		}

		parent, found := util.GetParent(a.Client, value.Pod.ObjectMeta)
		if found {
			currentAnalysis.ParentObject = parent
		}
		a.Results = append(a.Results, currentAnalysis)
	}

	return a.Results, nil
}

func analyzeContainerStatusFailures(a common.Analyzer, statuses []v1.ContainerStatus, name string, namespace string, statusPhase string) []common.Failure {
	var failures []common.Failure

	// Check through container status to check for crashes or unready
	for _, containerStatus := range statuses {
		if containerStatus.State.Waiting != nil {
			if containerStatus.State.Waiting.Reason == "ContainerCreating" && statusPhase == "Pending" {
				// This represents a container that is still being created or blocked due to conditions such as OOMKilled
				// parse the event log and append details
				evt, err := util.FetchLatestEvent(a.Context, a.Client, namespace, name)
				if err != nil || evt == nil {
					continue
				}
				if isEvtErrorReason(evt.Reason) && evt.Message != "" {
					failures = append(failures, common.Failure{
						Text:      evt.Message,
						Sensitive: []common.Sensitive{},
					})
				}
			} else if containerStatus.State.Waiting.Reason == "CrashLoopBackOff" && containerStatus.LastTerminationState.Terminated != nil {
				// This represents container that is in CrashLoopBackOff state due to conditions such as OOMKilled
				failures = append(failures, common.Failure{
					Text:      fmt.Sprintf("the last termination reason is %s container=%s pod=%s", containerStatus.LastTerminationState.Terminated.Reason, containerStatus.Name, name),
					Sensitive: []common.Sensitive{},
				})
			} else if isErrorReason(containerStatus.State.Waiting.Reason) && containerStatus.State.Waiting.Message != "" {
				failures = append(failures, common.Failure{
					Text:      containerStatus.State.Waiting.Message,
					Sensitive: []common.Sensitive{},
				})
			}
		} else {
			// when pod is Running but its ReadinessProbe fails
			if !containerStatus.Ready && statusPhase == "Running" {
				// parse the event log and append details
				evt, err := util.FetchLatestEvent(a.Context, a.Client, namespace, name)
				if err != nil || evt == nil {
					continue
				}
				if evt.Reason == "Unhealthy" && evt.Message != "" {
					failures = append(failures, common.Failure{
						Text:      evt.Message,
						Sensitive: []common.Sensitive{},
					})
				}
			}
		}
	}

	return failures
}

func isErrorReason(reason string) bool {
	failureReasons := []string{
		"CrashLoopBackOff", "ImagePullBackOff", "CreateContainerConfigError", "PreCreateHookError", "CreateContainerError",
		"PreStartHookError", "RunContainerError", "ImageInspectError", "ErrImagePull", "ErrImageNeverPull", "InvalidImageName",
	}

	for _, r := range failureReasons {
		if r == reason {
			return true
		}
	}
	return false
}

func isEvtErrorReason(reason string) bool {
	failureReasons := []string{
		"FailedCreatePodSandBox", "FailedMount",
	}

	for _, r := range failureReasons {
		if r == reason {
			return true
		}
	}
	return false
}

func (p PodAnalyzer) GetFixedYAML(result *common.Result, aiProvider common.IAI, ctx context.Context) (string, error) {
	// Build error context
	var errorMsgs []string
	errorComponents := make(map[string]bool)

	for _, failure := range result.Error {
		errorMsgs = append(errorMsgs, fmt.Sprintf("- %s", failure.Text))

		// Identify affected components
		switch {
		case strings.Contains(failure.Text, "image"):
			errorComponents["image"] = true
		case strings.Contains(failure.Text, "ConfigMap"):
			errorComponents["configmap"] = true
		case strings.Contains(failure.Text, "probe"):
			errorComponents["healthcheck"] = true
		case strings.Contains(failure.Text, "OOMKilled"):
			errorComponents["resources"] = true
		}
	}

	// Build the prompt
	prompt := fmt.Sprintf(`Given a Kubernetes Pod with issues:
Pod Name: %s
Issues Found:
%s

Please provide a corrected Pod YAML that:
1. ONLY contains the YAML manifest
2. Starts with 'apiVersion: v1'
3. Uses kind: Pod
4. Keeps the pod name: %s
5. Fixes all identified issues`,
		result.Name,
		strings.Join(errorMsgs, "\n"),
		strings.TrimPrefix(result.Name, "default/"))

	// Add component-specific guidance if needed
	if len(errorComponents) > 0 {
		prompt += "\n\nPay special attention to:"
		if errorComponents["image"] {
			prompt += "\n- Image references and pull policies"
		}
		if errorComponents["configmap"] {
			prompt += "\n- ConfigMap references and mounting"
		}
		if errorComponents["healthcheck"] {
			prompt += "\n- Health check probe configuration"
		}
		if errorComponents["resources"] {
			prompt += "\n- Resource limits and requests"
		}
	}

	fixedYAML, err := aiProvider.GetCompletion(ctx, prompt)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(fixedYAML), nil
}
