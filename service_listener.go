package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"strings"
	"time"
	"net/http"
	"crypto/tls"
	"encoding/json"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"
)

type Services struct {
	Items []Service
}

type Service struct {
	Metadata Metadata
	Status Status
}

type Metadata struct {
	Annotations Annotations
	Name string
}

type Annotations struct {
	DomainName string
}

type Status struct {
	LoadBalancer LoadBalancer
}

type LoadBalancer struct {
	Ingress []Ingress
}

type Ingress struct {
	Hostname string
}

func main() {
	flag.Parse()
	glog.Info("Route53 Update Service")

	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	token, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		glog.Fatalf("No service account token found")
	}

	httpTransport := &http.Transport{
        TLSClientConfig: &tls.Config{
    		InsecureSkipVerify: true,
    	},
    }
	httpClient := &http.Client{
		Transport: httpTransport,
	}
	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		glog.Fatalf("Please provide namespace")
	}
	serviceUrl := fmt.Sprintf("https://kubernetes.default/api/v1/namespaces/%s/services?labelSelector=dns=route53", namespace)
	httpRequest, err := http.NewRequest("GET", serviceUrl, nil)
	if err != nil {
		glog.Fatalf("Failed to build http request: %v", err)
	} 
	httpRequest.Header.Add("Authorization", 
		fmt.Sprintf("Bearer %s", string(token)))

	//aws credentials provided through env (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)
	creds := credentials.NewChainCredentials(
		[]credentials.Provider{
			&credentials.EnvProvider{},
		})

	//aws region provided through env (AWS_REGION)
	awsConfig := aws.Config{
		Credentials: creds,
	}

	awsSession := session.New()

	r53Api := route53.New(awsSession, &awsConfig)
	elbApi := elb.New(awsSession, &awsConfig)

	glog.Infof("Starting Service Polling every 30s")
	for {
		httpResponse, err := httpClient.Do(httpRequest)
		if err != nil {
			glog.Fatalf("Failed to list services: %v", err)
		}

		if httpResponse.StatusCode != 200 {
			glog.Fatalf("Bad response status: %d", httpResponse.StatusCode)
		}

		defer httpResponse.Body.Close()
		decoder := json.NewDecoder(httpResponse.Body)
		var services Services
		err = decoder.Decode(&services)
		if err != nil {
			glog.Fatalf("Failed to decode json: %v", err)
		}

		glog.Infof("Found %d DNS services in namespace %s", len(services.Items), namespace)
		for i := range services.Items {
			s := &services.Items[i]
			ingress := s.Status.LoadBalancer.Ingress
			if len(ingress) < 1 {
				glog.Warningf("No ingress defined for ELB")
				continue
			}
			if len(ingress) < 1 {
				glog.Warningf("Multiple ingress points found for ELB, not supported")
				continue
			}
			hn := ingress[0].Hostname

			domain := s.Metadata.Annotations.DomainName
			if len(domain) < 1 {
				glog.Warningf("Domain name not set for %s", s.Metadata.Name)
				continue
			}

			glog.Infof("Creating DNS for %s service: %s -> %s", s.Metadata.Name, hn, domain)
			tld := os.Getenv("TLD")
			if tld == "" {
				glog.Fatalf("Please provide tld")
			}

			elbName := strings.Split(hn, "-")[0]
			lbInput := &elb.DescribeLoadBalancersInput{
				LoadBalancerNames: []*string{
					&elbName,
				},
			}
			resp, err := elbApi.DescribeLoadBalancers(lbInput)
			if err != nil {
				glog.Warningf("Could not describe load balancer: %v", err)
				continue
			}
			descs := resp.LoadBalancerDescriptions
			if len(descs) < 1 {
				glog.Warningf("No lb found for %s: %v", tld, err)
				continue
			}
			if len(descs) > 1 {
				glog.Warningf("Multiple lbs found for %s: %v", tld, err)
				continue
			}
			hzId := descs[0].CanonicalHostedZoneNameID

			listHostedZoneInput := route53.ListHostedZonesByNameInput{
				DNSName: &tld,
			}
			hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
			if err != nil {
				glog.Warningf("No zone found for %s: %v", tld, err)
				continue
			}
			zones := hzOut.HostedZones
			if len(zones) < 1 {
				glog.Warningf("No zone found for %s", tld)
				continue
			}
			// The AWS API may return more than one zone, the first zone should be the relevant one
			tldWithDot := fmt.Sprint(tld, ".")
			if *zones[0].Name != tldWithDot {
				glog.Warningf("Zone found %s does not match tld given %s", *zones[0].Name, tld)
				continue
			}
			zoneId := *zones[0].Id
			zoneParts := strings.Split(zoneId, "/")
			zoneId = zoneParts[len(zoneParts)-1]

			at := route53.AliasTarget{
				DNSName:              &hn,
				EvaluateTargetHealth: aws.Bool(false),
				HostedZoneId:         hzId,
			}
			rrs := route53.ResourceRecordSet{
				AliasTarget: &at,
				Name:        &domain,
				Type:        aws.String("A"),
			}
			change := route53.Change{
				Action:            aws.String("UPSERT"),
				ResourceRecordSet: &rrs,
			}
			batch := route53.ChangeBatch{
				Changes: []*route53.Change{&change},
				Comment: aws.String("Kubernetes Update to Service"),
			}
			crrsInput := route53.ChangeResourceRecordSetsInput{
				ChangeBatch:  &batch,
				HostedZoneId: &zoneId,
			}
			_, err = r53Api.ChangeResourceRecordSets(&crrsInput)
			if err != nil {
				glog.Warningf("Failed to update record set: %v", err)
				continue
			}
		}
		time.Sleep(30 * time.Second)
	}
}
