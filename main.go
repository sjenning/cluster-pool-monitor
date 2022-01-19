package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"

	cr "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
)

var (
	Scheme         = runtime.NewScheme()
	YamlSerializer = json.NewSerializerWithOptions(
		json.DefaultMetaFactory, Scheme, Scheme,
		json.SerializerOptions{Yaml: true, Pretty: true, Strict: true},
	)
)

// GetConfigOrDie creates a REST config from current context
func GetConfigOrDie() *rest.Config {
	cfg := cr.GetConfigOrDie()
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg
}

// GetClientOrDie creates a controller-runtime client for Kubernetes
func GetClientOrDie() crclient.Client {
	client, err := crclient.New(GetConfigOrDie(), crclient.Options{Scheme: Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to get client: %v", err)
		os.Exit(1)
	}
	return client
}

func init() {
	corev1.AddToScheme(Scheme)
	hivev1.AddToScheme(Scheme)
}

const htmlPage = `
<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8"><title>Hypershift Cluster Pool Status</title>
<link rel="stylesheet" href="https://stackpath.bootstrapcdn.com/bootstrap/4.1.3/css/bootstrap.min.css" integrity="sha384-MCw98/SFnGE8fJT3GXwEOngsV7Zt27NXFoaoApmYm81iuXoPkFOJwJ8ERdknLPMO" crossorigin="anonymous">
<style>

</style>
</head>
<body>
<div class="container-fluid">
	{{ range .Pools }}
	<h3>{{ .Name }} Cluster Pool</h3>
	<table class="table">
		<thead class="thead-light">
			<tr>
				<th scope="col">CurrentSize</th>
				<th scope="col">MaxSize</th>
				<th scope="col">Unclaimed</th>
				<th scope="col">Ready</th>
			</tr>
		</thead>
		<tbody>
			<tr>
				<td>{{ .CurrentSize }}</td>
				<td>{{ .MaxSize }}</td>
				<td>{{ .Unclaimed }}</td>
				<td>{{ .Ready }}</td>
			</tr>
		</tbody>
	</table>
	<table class="table">
		<thead class="thead-light">
			<tr>
				<th scope="col">InfraID</th>
				<th scope="col">Status</th>
				<th scope="col">PowerState</th>
				<th scope="col">ProwState</th>
				<th scope="col">Prow</th>
			</tr>
		</thead>
		<tbody>
			{{ range .Deployments}}
			<tr>
				<td scope="row"><a href="https://console.aws.amazon.com/ec2/v2/home?region=us-east-1#Instances:v=3;search=:{{ .InfraID }}">{{ .InfraID }}</a></td>
				<td>{{ .Status }}</td>
				<td>{{ .PowerState }}</td>
				<td>{{ .ProwState }}</td>
				{{ if .ProwJob }}
				<td><a href="{{ .ProwURL }}">{{ .ProwJobName }}</a>{{ if .ProwPullAuthor}} - {{ .ProwPullAuthor }} (<a title="{{ .ProwPullTitle }}" href="{{ .ProwPullLink }}">{{ .ProwPullNumber }}</a>){{ end }}</td>
				{{ else }}
				<td>Unclaimed</td>
				{{ end }}
			</tr>
			{{ end }}
		</tbody>
	</table>
	{{ end }}
</div>
</body>
</html>
`

type ClusterDeployment struct {
	Name           string
	InfraID        string
	Status         string
	PowerState     string
	ProwJob        string
	ProwJobName    string
	ProwPullAuthor string
	ProwPullNumber string
	ProwPullLink   string
	ProwPullTitle  string
	ProwState      string
	ProwURL        string
}

type ClusterPool struct {
	Name        string
	CurrentSize int32
	MaxSize     *int32
	Unclaimed   int32
	Ready       int32
	Deployments []ClusterDeployment
}

type StatusPage struct {
	Pools []ClusterPool
}

type ProwData struct {
	Spec struct {
		Job  string `yaml:"job"`
		Refs struct {
			Pulls []struct {
				Author string `yaml:"author"`
				Link   string `yaml:"link"`
				Number string `yaml:"number"`
				Title  string `yaml:"title"`
			} `yaml:"pulls"`
		} `yaml:"refs"`
	} `yaml:"spec"`
	Status struct {
		State string `yaml:"state"`
		URL   string `yaml:"url"`
	} `yaml:"status"`
}

func getData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html;charset=UTF-8")
	page := template.Must(template.New("page").Parse(htmlPage))

	ctx := context.Background()

	clusterPoolList := &hivev1.ClusterPoolList{}
	client.List(ctx, clusterPoolList, crclient.InNamespace("hypershift-cluster-pool"))
	clusterClaimList := &hivev1.ClusterClaimList{}
	client.List(ctx, clusterClaimList, crclient.InNamespace("hypershift-cluster-pool"))
	var clusterPools []ClusterPool
	for _, clusterpool := range clusterPoolList.Items {
		var deployments []ClusterDeployment
		namespaceList := &corev1.NamespaceList{}
		client.List(ctx, namespaceList)
		for _, ns := range namespaceList.Items {
			if strings.HasPrefix(ns.Name, clusterpool.Name) {
				clusterDeploymentList := &hivev1.ClusterDeploymentList{}
				client.List(ctx, clusterDeploymentList, crclient.InNamespace(ns.Name))
				clusterDeployment := clusterDeploymentList.Items[0]
				var status string
				for _, condition := range clusterDeployment.Status.Conditions {
					if condition.Type == hivev1.ProvisionedCondition {
						status = condition.Reason
						break
					}
				}
				var prowJob string
				for _, claim := range clusterClaimList.Items {
					if claim.Spec.Namespace == clusterDeployment.Namespace {
						prowJob = claim.Name
					}
				}
				var prowData ProwData
				deployment := ClusterDeployment{
					Name:       clusterDeployment.Name,
					InfraID:    clusterDeployment.Spec.ClusterMetadata.InfraID,
					Status:     status,
					PowerState: clusterDeployment.Status.PowerState,
					ProwJob:    prowJob,
				}
				if prowJob != "" {
					resp, _ := http.Get(fmt.Sprintf("https://prow.ci.openshift.org/prowjob?prowjob=%s", prowJob))
					bytes, _ := io.ReadAll(resp.Body)
					yaml.Unmarshal(bytes, &prowData)
					deployment.ProwJobName = prowData.Spec.Job
					if len(prowData.Spec.Refs.Pulls) > 0 {
						deployment.ProwPullAuthor = prowData.Spec.Refs.Pulls[0].Author
						deployment.ProwPullNumber = prowData.Spec.Refs.Pulls[0].Number
						deployment.ProwPullLink = prowData.Spec.Refs.Pulls[0].Link
						deployment.ProwPullTitle = prowData.Spec.Refs.Pulls[0].Title
					}
					deployment.ProwState = prowData.Status.State
					deployment.ProwURL = prowData.Status.URL
				}
				deployments = append(deployments, deployment)

			}
		}
		clusterPools = append(clusterPools, ClusterPool{
			Name:        clusterpool.Name,
			CurrentSize: clusterpool.Spec.Size,
			MaxSize:     clusterpool.Spec.MaxSize,
			Ready:       clusterpool.Status.Ready,
			Unclaimed:   clusterpool.Status.Size,
			Deployments: deployments,
		})
	}

	status := StatusPage{
		Pools: clusterPools,
	}

	page.Execute(w, status)
}

var client crclient.Client

func main() {
	client = GetClientOrDie()

	r := http.NewServeMux()
	r.HandleFunc("/", getData)

	log.Fatal(http.ListenAndServe(":8080", r))
}
