package main

import (
	"bytes"
	_ "embed"
	"html/template"
	"log"
	"os/signal"
	"syscall"
	"time"

	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"
	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"

	cr "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

var (
	Scheme         = runtime.NewScheme()
	YamlSerializer = json.NewSerializerWithOptions(
		json.DefaultMetaFactory, Scheme, Scheme,
		json.SerializerOptions{Yaml: true, Pretty: true, Strict: true},
	)
)

func init() {
	corev1.AddToScheme(Scheme)
	hivev1.AddToScheme(Scheme)
	prowv1.AddToScheme(Scheme)
}

//go:embed page.html
var htmlPage string

type ClusterDeployment struct {
	Name           string
	InfraID        string
	Status         string
	PowerState     string
	ProwJob        string
	ProwJobName    string
	ProwPullAuthor string
	ProwPullNumber int
	ProwPullLink   string
	ProwPullTitle  string
	ProwState      string
	ProwURL        string
}

type ClusterPool struct {
	Name        string
	Size        int32
	MaxSize     *int32
	Unclaimed   int32
	Ready       int32
	Deployments []ClusterDeployment
}

type StatusPage struct {
	Pools      []ClusterPool
	UpdateTime string
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
	client, err := crclient.New(cr.GetConfigOrDie(), crclient.Options{Scheme: Scheme})
	if err != nil {
		log.Fatalf("unable to get client: %v", err)
		os.Exit(1)
	}

	err = gatherData(ctx, client)
	if err != nil {
		log.Printf("error updating data: %v", err)
	} else {
		log.Print("data updated")
	}

	for {
		select {
		case <-time.After(time.Minute):
			err := gatherData(ctx, client)
			if err != nil {
				log.Printf("error updating data: %v", err)
			} else {
				log.Print("data updated")
			}
		case <-ctx.Done():
			os.Exit(0)
		}
	}
}

func gatherData(ctx context.Context, client crclient.Client) error {
	log.Print("updating data")

	clusterPoolList := &hivev1.ClusterPoolList{}
	err := client.List(ctx, clusterPoolList, crclient.InNamespace("hypershift-cluster-pool"))
	if err != nil {
		return fmt.Errorf("unable to list cluster pools: %v", err)
	}
	clusterClaimList := &hivev1.ClusterClaimList{}
	err = client.List(ctx, clusterClaimList, crclient.InNamespace("hypershift-cluster-pool"))
	if err != nil {
		return fmt.Errorf("unable to list cluster claims: %v", err)
	}
	var clusterPools []ClusterPool
	for _, clusterpool := range clusterPoolList.Items {
		var deployments []ClusterDeployment
		namespaceList := &corev1.NamespaceList{}
		err = client.List(ctx, namespaceList)
		if err != nil {
			return fmt.Errorf("unable to list namespaces: %v", err)
		}
		for _, ns := range namespaceList.Items {
			if !strings.HasPrefix(ns.Name, clusterpool.Name) {
				continue
			}
			clusterDeploymentList := &hivev1.ClusterDeploymentList{}
			err = client.List(ctx, clusterDeploymentList, crclient.InNamespace(ns.Name))
			if err != nil {
				return fmt.Errorf("unable to list cluster deployments: %v", err)
			}
			clusterDeployment := clusterDeploymentList.Items[0]
			var status string
			for _, condition := range clusterDeployment.Status.Conditions {
				if condition.Type == hivev1.ProvisionedCondition {
					status = condition.Reason
					break
				}
			}
			var prowJobID string
			for _, claim := range clusterClaimList.Items {
				if claim.Spec.Namespace == clusterDeployment.Namespace {
					prowJobID = claim.Name
				}
			}

			deployment := ClusterDeployment{
				Name:       clusterDeployment.Name,
				InfraID:    clusterDeployment.Spec.ClusterMetadata.InfraID,
				Status:     status,
				PowerState: clusterDeployment.Status.PowerState,
				ProwJob:    prowJobID,
			}
			if clusterDeployment.Spec.ClusterMetadata != nil {
				deployment.InfraID = clusterDeployment.Spec.ClusterMetadata.InfraID
			}
			if prowJobID != "" {
				var prowJob prowv1.ProwJob
				resp, err := http.Get(fmt.Sprintf("https://prow.ci.openshift.org/prowjob?prowjob=%s", prowJobID))
				if err != nil {
					return fmt.Errorf("unable to get prow job: %v", err)
				}
				bytes, err := io.ReadAll(resp.Body)
				if err != nil {
					return fmt.Errorf("unable to read prow job: %v", err)
				}
				YamlSerializer.Decode(bytes, nil, &prowJob)
				deployment.ProwJobName = prowJob.Spec.Job
				if prowJob.Spec.Refs != nil && len(prowJob.Spec.Refs.Pulls) > 0 {
					deployment.ProwPullAuthor = prowJob.Spec.Refs.Pulls[0].Author
					deployment.ProwPullNumber = prowJob.Spec.Refs.Pulls[0].Number
					deployment.ProwPullLink = prowJob.Spec.Refs.Pulls[0].Link
					deployment.ProwPullTitle = prowJob.Spec.Refs.Pulls[0].Title
				}
				deployment.ProwState = string(prowJob.Status.State)
				deployment.ProwURL = prowJob.Status.URL
			}
			deployments = append(deployments, deployment)
		}
		clusterPools = append(clusterPools, ClusterPool{
			Name:        clusterpool.Name,
			Size:        clusterpool.Spec.Size,
			MaxSize:     clusterpool.Spec.MaxSize,
			Ready:       clusterpool.Status.Ready,
			Unclaimed:   clusterpool.Status.Size,
			Deployments: deployments,
		})
	}

	statusPage := StatusPage{
		Pools:      clusterPools,
		UpdateTime: time.Now().Format(time.RFC822Z),
	}

	var output bytes.Buffer
	page := template.Must(template.New("page").Parse(htmlPage))
	err = page.Execute(&output, statusPage)
	if err != nil {
		return fmt.Errorf("unable to execute template: %v", err)
	}

	awsConfig := aws.NewConfig()
	awsConfig.Region = aws.String("us-east-1")
	awsSession := session.Must(session.NewSession())
	s3Client := s3.New(awsSession, awsConfig)
	_, err = s3Client.PutObject(&s3.PutObjectInput{
		ACL:          aws.String("public-read"),
		Body:         bytes.NewReader(output.Bytes()),
		Bucket:       aws.String("cluster-pool-monitor.hypershift.devcluster.openshift.com"),
		Key:          aws.String("index.html"),
		ContentType:  aws.String("text/html"),
		CacheControl: aws.String("max-age=50"),
	})
	if err != nil {
		return fmt.Errorf("unable to upload to s3: %v", err)
	}
	return nil
}
