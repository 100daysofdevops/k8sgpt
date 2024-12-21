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
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/k8sgpt-ai/k8sgpt/pkg/common"
	"github.com/k8sgpt-ai/k8sgpt/pkg/integration"
	"github.com/k8sgpt-ai/k8sgpt/pkg/kubernetes"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/spf13/viper"
)

var (
	AnalyzerErrorsMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "analyzer_errors",
		Help: "Number of errors detected by analyzer",
	}, []string{"analyzer_name", "object_name", "namespace"})
)

var coreAnalyzerMap = map[string]common.IAnalyzer{
	"Pod":                            PodAnalyzer{},
	"Deployment":                     DeploymentAnalyzer{},
	"ReplicaSet":                     ReplicaSetAnalyzer{},
	"PersistentVolumeClaim":          PvcAnalyzer{},
	"Service":                        ServiceAnalyzer{},
	"Ingress":                        IngressAnalyzer{},
	"StatefulSet":                    StatefulSetAnalyzer{},
	"CronJob":                        CronJobAnalyzer{},
	"Node":                           NodeAnalyzer{},
	"ValidatingWebhookConfiguration": ValidatingWebhookAnalyzer{},
	"MutatingWebhookConfiguration":   MutatingWebhookAnalyzer{},
}

var additionalAnalyzerMap = map[string]common.IAnalyzer{
	"HorizontalPodAutoScaler": HpaAnalyzer{},
	"PodDisruptionBudget":     PdbAnalyzer{},
	"NetworkPolicy":           NetworkPolicyAnalyzer{},
	"Log":                     LogAnalyzer{},
	"GatewayClass":            GatewayClassAnalyzer{},
	"Gateway":                 GatewayAnalyzer{},
	"HTTPRoute":               HTTPRouteAnalyzer{},
}

type Analyzer struct {
	AI            common.IAI
	analyzers     []common.IAnalyzer
	client        *kubernetes.Client
	namespace     string
	labelSelector string
	Context       context.Context
}

func NewAnalyzer(aiProvider common.IAI, analyzers []common.IAnalyzer, client *kubernetes.Client, namespace string, labelSelector string) *Analyzer {
	return &Analyzer{
		AI:            aiProvider,
		analyzers:     analyzers,
		client:        client,
		namespace:     namespace,
		labelSelector: labelSelector,
		Context:       context.Background(),
	}
}

func ListFilters() ([]string, []string, []string) {
	coreKeys := make([]string, 0, len(coreAnalyzerMap))
	for k := range coreAnalyzerMap {
		coreKeys = append(coreKeys, k)
	}

	additionalKeys := make([]string, 0, len(additionalAnalyzerMap))
	for k := range additionalAnalyzerMap {
		additionalKeys = append(additionalKeys, k)
	}

	integrationProvider := integration.NewIntegration()
	var integrationAnalyzers []string

	for _, i := range integrationProvider.List() {
		b, _ := integrationProvider.IsActivate(i)
		if b {
			in, err := integrationProvider.Get(i)
			if err != nil {
				fmt.Println(color.RedString(err.Error()))
				os.Exit(1)
			}
			integrationAnalyzers = append(integrationAnalyzers, in.GetAnalyzerName()...)
		}
	}

	return coreKeys, additionalKeys, integrationAnalyzers
}

func GetAnalyzerMap() (map[string]common.IAnalyzer, map[string]common.IAnalyzer) {

	coreAnalyzer := make(map[string]common.IAnalyzer)
	mergedAnalyzerMap := make(map[string]common.IAnalyzer)

	// add core analyzer
	for key, value := range coreAnalyzerMap {
		coreAnalyzer[key] = value
		mergedAnalyzerMap[key] = value
	}

	// add additional analyzer
	for key, value := range additionalAnalyzerMap {
		mergedAnalyzerMap[key] = value
	}

	integrationProvider := integration.NewIntegration()

	for _, i := range integrationProvider.List() {
		b, err := integrationProvider.IsActivate(i)
		if err != nil {
			fmt.Println(color.RedString(err.Error()))
			os.Exit(1)
		}
		if b {
			in, err := integrationProvider.Get(i)
			if err != nil {
				fmt.Println(color.RedString(err.Error()))
				os.Exit(1)
			}
			in.AddAnalyzer(&mergedAnalyzerMap)
		}
	}

	return coreAnalyzer, mergedAnalyzerMap
}

func (a *Analyzer) GetFixedYAML(result *common.Result) (string, error) {
	if a.AI == nil {
		return "", fmt.Errorf("AI provider not initialized")
	}

	//Here we are going to construct the prompt for AI
	var errorMsgs []string
	for _, failure := range result.Error {
		errorMsgs = append(errorMsgs, failure.Text)
	}
	errorText := strings.Join(errorMsgs, "\n")
	fmt.Printf("This is a error text which need a fix: %s\n", errorText)

	prompt := fmt.Sprintf(`Given the following Kubernetes error for %s '%s':
Error: %s

Please provide the correct Kubernetes YAML manifest with the fix of the issue
The response should:
1. Only contain the YAML manifest
2. Start with 'apiVersion:'
3. Use the same resource name
4. Fix the specific error mentioned
5. Include all necessary fields for the resource type`,
		result.Kind, result.Name, errorText)

	// Get a Kubernetes yaml manifest from AI provider
	fixedYAML, err := a.AI.GetCompletion(a.Context, prompt)
	if err != nil {
		fmt.Printf("Error getting AI completion: %v\n", err)
		return "", err
	}
	// It's time to clean up the YAML
	fixedYAML = strings.TrimSpace(fixedYAML)
	if !strings.HasPrefix(fixedYAML, "apiVersion:") {
		fmt.Printf("Warning: AI response doesn't start with apiVersion, trying to clean...\n")
		if idx := strings.Index(fixedYAML, "apiVersion:"); idx >= 0 {
			fixedYAML = fixedYAML[idx:]
		}
	}
	return fixedYAML, nil
}

func (a *Analyzer) AnalyzeResults(ctx context.Context, filters []string) error {
	results, err := a.GetResults(ctx, filters)
	if err != nil {
		return err
	}
	fmt.Printf("Got %d results to analyze\n", len(results))

	for i := range results {
		fmt.Printf("\nProcessing result %d: %s/%s\n", i, results[i].Kind, results[i].Name)

		if viper.GetBool("explain") {
			fmt.Printf("Getting explanation...\n")
			explanation, err := a.GetExplanation(&results[i])
			if err != nil {
				return err
			}
			results[i].Explanation = explanation
			fmt.Printf("Got explanation of length: %d\n", len(explanation))
		}

		// Handle any fixes
		if err := a.HandleFixes(&results[i]); err != nil {
			fmt.Printf("Error handling fixes: %v\n", err)
			return err
		}
	}

	// Output results
	fmt.Printf("\nOutputting %d results...\n", len(results))
	for i := range results {
		a.OutputResult(i, &results[i])
	}
	return nil
}

func (a *Analyzer) HandleFixes(result *common.Result) error {
	if !viper.GetBool("fix") || !viper.GetBool("explain") {
		return nil
	}

	if result.Kind == "Pod" {
		podAnalyzer := PodAnalyzer{}
		fixedYAML, err := podAnalyzer.GetFixedYAML(result, a.AI, a.Context)
		if err != nil {
			return err
		}
		result.FixedYAML = fixedYAML
	}
	return nil
}

func (a *Analyzer) OutputResult(i int, result *common.Result) {
	for _, err := range result.Error {
		fmt.Printf("- Error: %s\n", err.Text)
	}

	if result.Explanation != "" {
		fmt.Printf("\n%s\n", result.Explanation)
	}

	fmt.Printf("FixedYAML present: %v\n", result.FixedYAML != "")
	if result.FixedYAML != "" {
		fmt.Printf("\nOriginal YAML:\n%s\n", result.FixedYAML)

		// Create a clean YAML string
		cleanYAML := strings.TrimSpace(result.FixedYAML)
		cleanYAML = strings.ReplaceAll(cleanYAML, "    # Fixed image name", "")
		fmt.Printf("\nCleaned YAML:\n%s\n", cleanYAML)

		// Save fixed YAML to file
		filename := fmt.Sprintf("fixed-%s-%s.yaml", result.Kind, result.Name)
		fmt.Printf("Attempting to save to file: %s\n", filename)

		currentDir, _ := os.Getwd()
		fmt.Printf("Current directory: %s\n", currentDir)

		// Try to create the file
		err := os.WriteFile(filename, []byte(cleanYAML+"\n"), 0644)
		if err != nil {
			fmt.Printf("Error saving file: %v\n", err)
			return
		}

		// Verify the file was created
		if _, err := os.Stat(filename); err != nil {
			fmt.Printf("File verification failed: %v\n", err)
		} else {
			fmt.Printf("File successfully created and verified\n")
		}
		fmt.Printf("\nFixed YAML has been saved to: %s\n", filename)
	}
}

func (a *Analyzer) GetResults(ctx context.Context, filters []string) ([]common.Result, error) {
	var results []common.Result

	// Get results from analyzers
	for _, analyzer := range a.analyzers {
		// Create analyzer context using common.Analyzer
		analyzerCtx := common.Analyzer{
			Client:        a.client,
			Context:       ctx,
			Namespace:     a.namespace,
			LabelSelector: a.labelSelector,
		}

		analysisResults, err := analyzer.Analyze(analyzerCtx)
		if err != nil {
			return nil, err
		}
		results = append(results, analysisResults...)
	}

	return results, nil
}

func (a *Analyzer) GetExplanation(result *common.Result) (string, error) {
	if a.AI == nil {
		return "", fmt.Errorf("AI provider not initialized")
	}

	// Combine all failure messages
	var errorMsgs []string
	for _, failure := range result.Error {
		errorMsgs = append(errorMsgs, failure.Text)
	}
	errorText := strings.Join(errorMsgs, "\n")

	prompt := fmt.Sprintf(`Given the following Kubernetes error:
Error: %s
Context: %s

Please explain the problem and provide a solution.`,
		errorText, result.Name)

	explanation, err := a.AI.GetExplanation(prompt)
	if err != nil {
		return "", err
	}

	return explanation, nil
}

// Implement common.Analyzer interface methods
func (a *Analyzer) GetClient() interface{} {
	return a.client
}

func (a *Analyzer) GetNamespace() string {
	return a.namespace
}

func (a *Analyzer) GetLabelSelector() string {
	return a.labelSelector
}
