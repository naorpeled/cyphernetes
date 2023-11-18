package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oliveagle/jsonpath"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type QueryExecutor struct {
	Clientset     *kubernetes.Clientset
	DynamicClient dynamic.Interface
}

func NewQueryExecutor() (*QueryExecutor, error) {
	// Use the local kubeconfig context
	config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err != nil {
		fmt.Println("Error creating in-cluster config")
		return nil, err
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println("Error creating clientset")
		return nil, err
	}

	// Create the dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Println("Error creating dynamic client")
		return nil, err
	}

	return &QueryExecutor{Clientset: clientset, DynamicClient: dynamicClient}, nil
}

func (q *QueryExecutor) getK8sResources(kind string, fieldSelector string, labelSelector string) (unstructured.UnstructuredList, error) {
	// Use discovery client to find the GVR for the given kind
	gvr, err := findGVR(q.Clientset, kind)
	if err != nil {
		var emptyList unstructured.UnstructuredList
		return emptyList, err
	}

	// Use dynamic client to list resources
	logDebug("Listing resources of kind:", kind, "with fieldSelector:", fieldSelector, "and labelSelector:", labelSelector)
	labelSelectorParsed, err := metav1.ParseToLabelSelector(labelSelector)
	if err != nil {
		fmt.Println("Error parsing label selector: ", err)
		var emptyList unstructured.UnstructuredList
		return emptyList, err
	}
	labelMap, err := metav1.LabelSelectorAsSelector(labelSelectorParsed)
	if err != nil {
		fmt.Println("Error converting label selector to label map: ", err)
		var emptyList unstructured.UnstructuredList
		return emptyList, err
	}

	if allNamespaces {
		Namespace = ""
	}
	list, err := q.DynamicClient.Resource(gvr).Namespace(Namespace).List(context.Background(), metav1.ListOptions{
		FieldSelector: fieldSelector,
		LabelSelector: labelMap.String(),
	})
	if err != nil {
		fmt.Println("Error getting list of resources: ", err)
		var emptyList unstructured.UnstructuredList
		return emptyList, err
	}
	return *list, err
}

func findGVR(clientset *kubernetes.Clientset, resourceIdentifier string) (schema.GroupVersionResource, error) {
	discoveryClient := clientset.Discovery()

	// Get the list of API resources
	apiResourceList, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		return schema.GroupVersionResource{}, err
	}

	// Normalize the resource identifier to lower case for case-insensitive comparison
	normalizedIdentifier := strings.ToLower(resourceIdentifier)

	for _, apiResource := range apiResourceList {
		for _, resource := range apiResource.APIResources {
			// Check if the resource name, kind, or short names match the specified identifier
			if strings.EqualFold(resource.Name, normalizedIdentifier) || // Plural name match
				strings.EqualFold(resource.Kind, resourceIdentifier) || // Kind name match
				containsIgnoreCase(resource.ShortNames, normalizedIdentifier) { // Short name match

				gv, err := schema.ParseGroupVersion(apiResource.GroupVersion)
				if err != nil {
					return schema.GroupVersionResource{}, err
				}
				return gv.WithResource(resource.Name), nil
			}
		}
	}

	return schema.GroupVersionResource{}, fmt.Errorf("resource identifier not found: %s", resourceIdentifier)
}

// Helper function to check if a slice contains a string, case-insensitive
func containsIgnoreCase(slice []string, str string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, str) {
			return true
		}
	}
	return false
}

// Initialize the results variable.
var results interface{}
var resultMap map[string]interface{}
var resultMapJson []byte

func (q *QueryExecutor) Execute(ast *Expression) (interface{}, error) {
	k8sResources := make(map[string]interface{})

	// Iterate over the clauses in the AST.
	for _, clause := range ast.Clauses {
		switch c := clause.(type) {
		case *MatchClause:
			for _, nodePattern := range c.Nodes {
				debugLog("Node pattern found. Name:", nodePattern.ResourceProperties.Name, "Kind:", nodePattern.ResourceProperties.Kind)
				getNodeResouces(nodePattern, q)
			}
			// case *CreateClause:
			// 	// Execute a Kubernetes create operation based on the CreateClause.
			// 	// ...
			// case *SetClause:
			// 	// Execute a Kubernetes update operation based on the SetClause.
			// 	// ...
			// case *DeleteClause:
			// 	// Execute a Kubernetes delete operation based on the DeleteClause.
			// 	// ...
		case *ReturnClause:
			var jsonData interface{}
			json.Unmarshal(resultMapJson, &jsonData)

			for _, jsonPath := range c.JsonPaths {
				// Ensure the JSONPath starts with '$'
				if !strings.HasPrefix(jsonPath, "$") {
					jsonPath = "$." + jsonPath
				}

				pathParts := strings.Split(jsonPath, ".")[1:]

				// Drill down to create nested map structure
				currentMap := k8sResources
				for i, part := range pathParts {
					if i == len(pathParts)-1 {
						// Last part: assign the result
						result, err := jsonpath.JsonPathLookup(jsonData, jsonPath)
						if err != nil {
							logDebug("Path not found:", jsonPath)
							result = []interface{}{}
						}
						currentMap[part] = result
					} else {
						// Intermediate parts: create nested maps
						if currentMap[part] == nil {
							currentMap[part] = make(map[string]interface{})
						}
						currentMap = currentMap[part].(map[string]interface{})
					}
				}
			}

		default:
			return nil, fmt.Errorf("unknown clause type: %T", c)
		}
	}

	return k8sResources, nil
}

func getNodeResouces(n *NodePattern, q *QueryExecutor) (err error) {
	if n.ResourceProperties.Properties != nil && len(n.ResourceProperties.Properties.PropertyList) > 0 {
		for i, prop := range n.ResourceProperties.Properties.PropertyList {
			if prop.Key == "namespace" || prop.Key == "metadata.namespace" {
				Namespace = prop.Value.(string)
				// Remove the namespace slice from the properties
				n.ResourceProperties.Properties.PropertyList = append(n.ResourceProperties.Properties.PropertyList[:i], n.ResourceProperties.Properties.PropertyList[i+1:]...)
			}
		}
	}

	var fieldSelector string
	var labelSelector string
	var hasNameSelector bool
	if n.ResourceProperties.Properties != nil {
		for _, prop := range n.ResourceProperties.Properties.PropertyList {
			if prop.Key == "name" || prop.Key == "metadata.name" {
				fieldSelector += fmt.Sprintf("metadata.name=%s,", prop.Value)
				hasNameSelector = true
			} else {
				if hasNameSelector {
					// both name and label selectors are specified, error out
					return fmt.Errorf("the 'name' selector can be used by itself or combined with 'namespace', but not with other label selectors")
				}
				labelSelector += fmt.Sprintf("%s=%s,", prop.Key, prop.Value)
			}
		}
		fieldSelector = strings.TrimSuffix(fieldSelector, ",")
		labelSelector = strings.TrimSuffix(labelSelector, ",")

	}

	// Get the list of resources of the specified kind.
	list, err := q.getK8sResources(n.ResourceProperties.Kind, fieldSelector, labelSelector)
	if err != nil {
		fmt.Println("Error getting list of resources: ", err)
		return err
	}

	var converted []map[string]interface{}
	for _, u := range list.Items {
		converted = append(converted, u.UnstructuredContent())
	}
	// Initialize results as a map if not already done
	if results == nil {
		results = make(map[string]interface{})
	}

	// Add the list to the results under the 'name' key
	resultMap = results.(map[string]interface{})
	resultMap[n.ResourceProperties.Name] = converted
	resultMapJson, err = json.Marshal(resultMap)
	if err != nil {
		fmt.Println("Error marshalling results to JSON: ", err)
		return err
	}
	return nil
}
