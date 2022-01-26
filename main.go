package main

import (
	"bytes"
	_ "embed"
	"flag"
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

type PullRef struct {
	Author string
	Number int
	Link   string
	Title  string
}

type ProwJob struct {
	Name    string
	State   string
	URL     string
	PullRef *PullRef
}
type ClusterClaim struct {
	Namespace  string
	Job        *ProwJob
	Deployment *ClusterDeployment
}

type ClusterDeployment struct {
	InfraID    string
	PowerState string
	Claim      *ClusterClaim
}

type ClusterPool struct {
	Name          string
	Size          int32
	Standby       int32
	Ready         int32
	Deployments   []*ClusterDeployment
	PendingClaims []*ClusterClaim
}

type StatusPage struct {
	Pools      []*ClusterPool
	UpdateTime string
}

type Options struct {
	OutputS3Bucket string
	OutputFilePath string
	OneShot        bool
}

func main() {
	var opts Options
	flag.StringVar(&opts.OutputS3Bucket, "output-s3-bucket", "cluster-pool-monitor.hypershift.devcluster.openshift.com", "output to s3 bucket")
	flag.StringVar(&opts.OutputFilePath, "output-file-path", "", "output to file")
	flag.BoolVar(&opts.OneShot, "one-shot", false, "only update the data once, then exit")
	flag.Parse()

	var writer Writer
	if opts.OutputFilePath != "" {
		writer = NewFileWriter(opts.OutputFilePath)
	} else {
		writer = NewS3Writer(opts.OutputS3Bucket)
	}

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

	err = updateData(ctx, client, writer)
	if err != nil {
		log.Printf("error updating data: %v", err)
	} else {
		log.Print("data updated")
	}
	if opts.OneShot {
		os.Exit(0)
	}

	for {
		select {
		case <-time.After(time.Minute):
			err = updateData(ctx, client, writer)
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

func updateData(ctx context.Context, client crclient.Client, w Writer) error {
	data, err := gatherData(ctx, client)
	if err != nil {
		return fmt.Errorf("error gathering data: %v", err)
	}
	err = w.Write(data)
	if err != nil {
		return fmt.Errorf("error writing data: %v", err)
	}
	return nil
}

func getProwJob(id string) (*ProwJob, error) {
	resp, err := http.Get(fmt.Sprintf("https://prow.ci.openshift.org/prowjob?prowjob=%s", id))
	if err != nil {
		return nil, fmt.Errorf("unable to get prow job: %v", err)
	}
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read prow job: %v", err)
	}
	var prowJob prowv1.ProwJob
	YamlSerializer.Decode(bytes, nil, &prowJob)

	var rtnJob ProwJob
	rtnJob.Name = prowJob.Name
	rtnJob.State = string(prowJob.Status.State)
	rtnJob.URL = prowJob.Status.URL
	if prowJob.Spec.Refs != nil && len(prowJob.Spec.Refs.Pulls) > 0 {
		rtnJob.PullRef = &PullRef{
			Author: prowJob.Spec.Refs.Pulls[0].Author,
			Number: prowJob.Spec.Refs.Pulls[0].Number,
			Link:   prowJob.Spec.Refs.Pulls[0].Link,
			Title:  prowJob.Spec.Refs.Pulls[0].Title,
		}
	}
	return &rtnJob, nil
}

func gatherData(ctx context.Context, client crclient.Client) ([]byte, error) {
	log.Print("updating data")

	clusterPoolList := &hivev1.ClusterPoolList{}
	err := client.List(ctx, clusterPoolList, crclient.InNamespace("hypershift-cluster-pool"))
	if err != nil {
		return nil, fmt.Errorf("unable to list cluster pools: %v", err)
	}
	clusterClaimList := &hivev1.ClusterClaimList{}
	err = client.List(ctx, clusterClaimList, crclient.InNamespace("hypershift-cluster-pool"))
	if err != nil {
		return nil, fmt.Errorf("unable to list cluster claims: %v", err)
	}

	claims := make(map[string][]*ClusterClaim)
	for _, clusterClaim := range clusterClaimList.Items {
		prowJob, err := getProwJob(clusterClaim.Name)
		if err != nil {
			return nil, fmt.Errorf("unable to get prow job: %v", err)
		}
		claims[clusterClaim.Spec.ClusterPoolName] = append(claims[clusterClaim.Spec.ClusterPoolName], &ClusterClaim{
			Namespace: clusterClaim.Spec.Namespace,
			Job:       prowJob,
		})
	}

	var clusterPools []*ClusterPool
	for _, clusterpool := range clusterPoolList.Items {
		var deployments []*ClusterDeployment
		var pendingClaims []*ClusterClaim
		namespaceList := &corev1.NamespaceList{}
		err = client.List(ctx, namespaceList)
		if err != nil {
			return nil, fmt.Errorf("unable to list namespaces: %v", err)
		}
		for _, ns := range namespaceList.Items {
			if !strings.HasPrefix(ns.Name, clusterpool.Name) {
				continue
			}
			clusterDeploymentList := &hivev1.ClusterDeploymentList{}
			err = client.List(ctx, clusterDeploymentList, crclient.InNamespace(ns.Name))
			if err != nil {
				return nil, fmt.Errorf("unable to list cluster deployments: %v", err)
			}
			clusterDeployment := clusterDeploymentList.Items[0]

			deployment := &ClusterDeployment{
				PowerState: string(clusterDeployment.Status.PowerState),
			}
			if clusterDeployment.Spec.ClusterMetadata != nil {
				deployment.InfraID = clusterDeployment.Spec.ClusterMetadata.InfraID
			}
			for _, claim := range claims[clusterpool.Name] {
				if claim.Namespace == clusterDeployment.Namespace {
					deployment.Claim = claim
					claim.Deployment = deployment
					break
				}
			}

			deployments = append(deployments, deployment)
		}

		for _, claim := range claims[clusterpool.Name] {
			if claim.Deployment == nil {
				pendingClaims = append(pendingClaims, claim)
			}
		}

		clusterPools = append(clusterPools, &ClusterPool{
			Name:          clusterpool.Name,
			Size:          clusterpool.Spec.Size,
			Ready:         clusterpool.Status.Ready,
			Standby:       clusterpool.Status.Standby,
			Deployments:   deployments,
			PendingClaims: pendingClaims,
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
		return nil, fmt.Errorf("unable to execute template: %v", err)
	}

	return output.Bytes(), nil
}

type Writer interface {
	Write([]byte) error
}

type S3Writer struct {
	client     *s3.S3
	bucketName string
}

func NewS3Writer(bucketName string) Writer {
	awsConfig := aws.NewConfig()
	awsConfig.Region = aws.String("us-east-1")
	awsSession := session.Must(session.NewSession())
	s3Client := s3.New(awsSession, awsConfig)
	return &S3Writer{
		client:     s3Client,
		bucketName: bucketName,
	}
}

func (w *S3Writer) Write(data []byte) error {
	_, err := w.client.PutObject(&s3.PutObjectInput{
		ACL:          aws.String("public-read"),
		Body:         bytes.NewReader(data),
		Bucket:       aws.String("cluster-pool-monitor.hypershift.devcluster.openshift.com"),
		Key:          aws.String("index.html"),
		ContentType:  aws.String("text/html"),
		CacheControl: aws.String("max-age=50"),
	})
	if err != nil {
		return fmt.Errorf("unable to write to s3: %v", err)
	}
	return nil
}

type FileWriter struct {
	FilePath string
}

func NewFileWriter(filePath string) Writer {
	return &FileWriter{
		FilePath: filePath,
	}
}

func (w *FileWriter) Write(data []byte) error {
	file, err := os.Create(w.FilePath)
	if err != nil {
		return fmt.Errorf("unable to create file: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("unable to write file: %v", err)
	}
	return nil
}
