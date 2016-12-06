package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
)

var (
	dest    string
	port    int
	region  string
	elbName string
	sleep   time.Duration
	tags    Tags
)

// TargetGroup is a collection of related hosts that prometheus monitors
type TargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

type Tag struct {
	Key         string
	FilterName  string
	FilterValue string
}
type Tags []Tag

// filter down the instance ids to only in service ones
func filterHealthyInstances(output *elb.DescribeInstanceHealthOutput) []*string {
	healthyInstances := []*string{}
	for _, instance := range output.InstanceStates {
		if *instance.State == "InService" {
			healthyInstances = append(healthyInstances, instance.InstanceId)
		}

	}

	return healthyInstances
}

func getTag(instance ec2.Instance, key string) string {
	for _, t := range instance.Tags {
		if *t.Key == key {
			return *t.Value
		}
	}
	return ""
}

func groupByTags(instances []*ec2.Instance, tags []string) map[string]*TargetGroup {
	targetGroups := make(map[string]*TargetGroup)

	for _, instance := range instances {
		if *instance.State.Code != 16 { // 16 = Running
			continue
		}

		key := ""
		for _, tagKey := range tags {
			key = fmt.Sprintf("%s|%s=%s", key, tagKey, getTag(*instance, tagKey))
		}

		targetGroup, ok := targetGroups[key]
		if !ok {
			labels := make(map[string]string)
			for _, tagKey := range tags {
				tagVal := getTag(*instance, tagKey)
				if tagVal != "" {
					labels[tagKey] = tagVal
				}
			}
			targetGroup = &TargetGroup{
				Labels:  labels,
				Targets: make([]string, 0),
			}
			targetGroups[key] = targetGroup
		}

		target := fmt.Sprintf("%s:%d", *instance.PrivateIpAddress, port)
		targetGroup.Targets = append(targetGroup.Targets, target)
	}

	return targetGroups
}

func marshalTargetGroups(targetGroups map[string]*TargetGroup) []byte {
	// We need to transform targetGroups into a values list sorted by key
	tgList := []*TargetGroup{}
	keys := []string{}
	for k, _ := range targetGroups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tgList = append(tgList, targetGroups[k])
	}

	b, err := json.MarshalIndent(tgList, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	return b
}

func atomicWriteFile(filename string, data []byte, tmpSuffix string) error {
	err := ioutil.WriteFile(filename+tmpSuffix, data, 0644)
	if err != nil {
		return err
	}
	err = os.Rename(filename+tmpSuffix, filename)
	if err != nil {
		return err
	}
	return nil
}

func flattenReservations(reservations []*ec2.Reservation) []*ec2.Instance {
	instances := make([]*ec2.Instance, 0)
	for _, r := range reservations {
		instances = append(instances, r.Instances...)
	}
	return instances
}

func parseTags(tagsRaw string) Tags {
	fields := strings.Split(tagsRaw, ",")
	if fields[0] == "" && len(fields) == 1 {
		return Tags{}
	}
	tags := make(Tags, len(fields))
	for i, t := range fields {
		parts := strings.Split(t, "=")
		switch len(parts) {
		case 1:
			tags[i] = Tag{
				Key:         t,
				FilterName:  "tag-key",
				FilterValue: t,
			}
		case 2:
			tags[i] = Tag{
				Key:         parts[0],
				FilterName:  "tag:" + parts[0],
				FilterValue: parts[1],
			}
		default:
			log.Fatalf("Unrecognized tag filter %v", t)
		}
	}
	return tags
}

func (tags Tags) Keys() []string {
	seen := map[string]bool{}
	keys := []string{}
	for _, t := range tags {
		if !seen[t.Key] {
			seen[t.Key] = true
			keys = append(keys, t.Key)
		}
	}
	return keys
}

func allTagKeys(instances []*ec2.Instance) []string {
	tagSet := map[string]struct{}{}
	for _, instance := range instances {
		for _, t := range instance.Tags {
			tagSet[*t.Key] = struct{}{}
		}
	}
	tags := []string{}
	for tag, _ := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func initFlags() {
	var (
		tagsRaw   string
		regionRaw string
		elbRaw    string
	)

	flag.IntVar(&port, "port", 80, "Port that is exposing /metrics")
	flag.StringVar(&dest, "dest", "-", "File to write the target group JSON. (e.g. `tgroups/target_groups.json`)")
	flag.StringVar(&regionRaw, "region", "us-west-2", "AWS region to query")
	flag.StringVar(&elbRaw, "elb", "", "Load Balancer to query")
	flag.StringVar(&tagsRaw, "tags", "Name", "Comma seperated list of tags to group by (e.g. `Environment,Application`). You can also filter by tag value (e.g. `Application,Envionment=Production`)")

	flag.Parse()
	tags = parseTags(tagsRaw)
	region = regionRaw
	elbName = elbRaw
}

func main() {

	initFlags()

	sess, err := session.NewSession()
	if err != nil {
		fmt.Println("failed to create session,", err)
		return
	}

	svc := elb.New(sess, &aws.Config{Region: &region})

	// fetch the instances from the ELB
	params := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{
			&elbName, // Required
			// More values...
		},
	}
	resp, err := svc.DescribeLoadBalancers(params)

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		fmt.Println(err.Error())
		return
	}

	// save the instances in to their own slice
	unCheckedInstances := []*elb.Instance{}
	for _, instance := range resp.LoadBalancerDescriptions[0].Instances {
		unCheckedInstances = append(unCheckedInstances, instance)
	}

	// get the health of previously found instances.
	// to determine which ones have nginx on them
	paramsHealth := &elb.DescribeInstanceHealthInput{
		LoadBalancerName: &elbName, // Required
		Instances:        unCheckedInstances,
	}

	respHealth, err := svc.DescribeInstanceHealth(paramsHealth)

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		fmt.Println(err.Error())
		return
	}

	// Pretty-print the response data.
	// fmt.Println(respHealth)

	// filter elb instances down to only the healthy ones
	healthyInstances := filterHealthyInstances(respHealth)

	// get the IP addresses of the healthy instances
	ec2sess, err := session.NewSession()
	if err != nil {
		fmt.Println("failed to create session,", ec2sess)
		return
	}

	ec2svc := ec2.New(ec2sess, &aws.Config{Region: aws.String("us-east-1")})
	ec2params := &ec2.DescribeInstancesInput{
		InstanceIds: healthyInstances,
	}
	ec2Resp, err := ec2svc.DescribeInstances(ec2params)

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		log.Println(err.Error())
		return
	}

	// Pretty-print the response data.
	// fmt.Println(ec2Resp)

	// flatten the instances array
	instances := flattenReservations(ec2Resp.Reservations)

	tagKeys := tags.Keys()
	if len(tagKeys) == 0 {
		tagKeys = allTagKeys(instances)
	}

	targetGroups := groupByTags(instances, tagKeys)
	b := marshalTargetGroups(targetGroups)
	if dest == "-" {
		_, err = os.Stdout.Write(b)
	} else {
		err = atomicWriteFile(dest, b, ".new")
	}
	if err != nil {
		log.Fatal(err)
	}
}
