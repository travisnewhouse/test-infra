/*
Copyright 2016 The Kubernetes Authors.

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
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/golang/glog"
)

var regionsRaw = flag.String("regions", "us-west-2", "Comma separated list of regions to query for resources")
var maxTTL = flag.Duration("ttl", 24*time.Hour, "Maximum time before we attempt deletion of a resource. Set to 0s to nuke all non-default resources.")
var path = flag.String("path", "", "S3 path to store mark data in (required)")

type awsResourceType interface {
	// MarkAndSweep queries the resource in a specific region, using
	// the provided session (which has account-number acct), calling
	// res.Mark(<resource>) on each resource and deleting
	// appropriately.
	MarkAndSweep(sess *session.Session, acct string, region string, res *awsResourceSet) error
}

// AWS resource types known to this script, in dependency order.
var awsResourceTypes = []awsResourceType{
	autoScalingGroups{},
	launchConfigurations{},
	instances{},
	// Addresses
	// NetworkInterfaces
	subnets{},
	securityGroups{},
	// NetworkACLs
	// VPN Connections
	internetGateways{},
	routeTables{},
	vpcs{},
	dhcpOptions{},
	volumes{},
}

type awsResource interface {
	// ARN returns the AWS ARN for the resource
	// (c.f. http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html). This
	// is only used for uniqueness in the Mark set, but ARNs are
	// intended to be globally unique across regions and accounts, so
	// that works.
	ARN() string
}

// awsResourceSet keeps track of the first time we saw a particular
// ARN, and the global TTL. See Mark() for more details.
type awsResourceSet struct {
	firstSeen map[string]time.Time // ARN -> first time we saw
	marked    map[string]bool      // ARN -> seen this run
	ttl       time.Duration
}

func LoadResourceSet(sess *session.Session, p *s3path, ttl time.Duration) (*awsResourceSet, error) {
	s := &awsResourceSet{firstSeen: make(map[string]time.Time), marked: make(map[string]bool), ttl: ttl}
	svc := s3.New(sess, &aws.Config{Region: aws.String(p.region)})
	resp, err := svc.GetObject(&s3.GetObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(p.key)})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == "NoSuchKey" {
			return s, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s.firstSeen); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *awsResourceSet) Save(sess *session.Session, p *s3path) error {
	s.markComplete()
	b, err := json.MarshalIndent(s.firstSeen, "", "  ")
	if err != nil {
		return err
	}
	svc := s3.New(sess, &aws.Config{Region: aws.String(p.region)})
	_, err = svc.PutObject(&s3.PutObjectInput{
		Bucket:       aws.String(p.bucket),
		Key:          aws.String(p.key),
		Body:         bytes.NewReader(b),
		CacheControl: aws.String("max-age=0"),
	})
	return err
}

// Mark marks a particular resource as currently present, and advises
// on whether it should be deleted. If Mark(r) returns true, the TTL
// has expired for r and it should be deleted.
func (s *awsResourceSet) Mark(r awsResource) bool {
	arn := r.ARN()
	now := time.Now()

	s.marked[arn] = true
	if t, ok := s.firstSeen[arn]; ok {
		since := now.Sub(t)
		if since > s.ttl {
			return true
		}
		glog.V(1).Infof("%s: seen for %v", r.ARN(), since)
		return false
	}
	s.firstSeen[arn] = now
	glog.V(1).Infof("%s: first seen", r.ARN())
	return s.ttl == 0 // If the TTL is 0, it should be deleted now.
}

// markComplete figures out which ARNs were in previous passes but not
// this one, and eliminates them. It should only be run after all
// resources have been marked.
func (s *awsResourceSet) markComplete() {
	var gone []string
	for arn, _ := range s.firstSeen {
		if !s.marked[arn] {
			gone = append(gone, arn)
		}
	}
	for _, arn := range gone {
		glog.V(1).Infof("%s: deleted since last run", arn)
		delete(s.firstSeen, arn)
	}
}

// Instances: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeInstances

type instances struct{}

func (instances) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	inp := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("instance-state-name"),
				Values: []*string{aws.String("running"), aws.String("pending")},
			},
		},
	}

	var toDelete []*string // Paged call, defer deletion until we have the whole list.
	if err := svc.DescribeInstancesPages(inp, func(page *ec2.DescribeInstancesOutput, _ bool) bool {
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				i := &instance{
					Account:    acct,
					Region:     region,
					InstanceID: *inst.InstanceId,
				}
				if set.Mark(i) {
					glog.Warningf("%s: deleting %T: %v", i.ARN(), inst, inst)
					toDelete = append(toDelete, inst.InstanceId)
				}
			}
		}
		return true
	}); err != nil {
		return err
	}
	if len(toDelete) > 0 {
		// TODO(zmerlynn): In theory this should be split up into
		// blocks of 1000, but burn that bridge if it ever happens...
		_, err := svc.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: toDelete})
		if err != nil {
			glog.Warningf("termination failed: %v (for %v)", err, toDelete)
		}
	}
	return nil
}

type instance struct {
	Account    string
	Region     string
	InstanceID string
}

func (i instance) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", i.Region, i.Account, i.InstanceID)
}

// AutoScalingGroups: https://docs.aws.amazon.com/sdk-for-go/api/service/autoscaling/#AutoScaling.DescribeAutoScalingGroups

type autoScalingGroups struct{}

func (autoScalingGroups) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := autoscaling.New(sess, &aws.Config{Region: aws.String(region)})

	var toDelete []*autoScalingGroup // Paged call, defer deletion until we have the whole list.
	if err := svc.DescribeAutoScalingGroupsPages(nil, func(page *autoscaling.DescribeAutoScalingGroupsOutput, _ bool) bool {
		for _, asg := range page.AutoScalingGroups {
			a := &autoScalingGroup{ID: *asg.AutoScalingGroupARN, Name: *asg.AutoScalingGroupName}
			if set.Mark(a) {
				glog.Warningf("%s: deleting %T: %v", a.ARN(), asg, asg)
				toDelete = append(toDelete, a)
			}
		}
		return true
	}); err != nil {
		return err
	}
	for _, asg := range toDelete {
		_, err := svc.DeleteAutoScalingGroup(
			&autoscaling.DeleteAutoScalingGroupInput{
				AutoScalingGroupName: aws.String(asg.Name),
				ForceDelete:          aws.Bool(true),
			})
		if err != nil {
			glog.Warningf("%v: delete failed: %v", asg.ARN(), err)
		}
	}
	// Block on ASGs finishing deletion. There are a lot of dependent
	// resources, so this just makes the rest go more smoothly (and
	// prevents a second pass).
	for _, asg := range toDelete {
		glog.Warningf("%v: waiting for delete", asg.ARN())
		err := svc.WaitUntilGroupNotExists(
			&autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []*string{aws.String(asg.Name)},
			})
		if err != nil {
			glog.Warningf("%v: wait failed: %v", asg.ARN(), err)
		}
	}
	return nil
}

type autoScalingGroup struct {
	ID   string
	Name string
}

func (asg autoScalingGroup) ARN() string {
	return asg.ID
}

// LaunchConfigurations: http://docs.aws.amazon.com/sdk-for-go/api/service/autoscaling/#AutoScaling.DescribeLaunchConfigurations

type launchConfigurations struct{}

func (launchConfigurations) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := autoscaling.New(sess, &aws.Config{Region: aws.String(region)})

	var toDelete []*launchConfiguration // Paged call, defer deletion until we have the whole list.
	if err := svc.DescribeLaunchConfigurationsPages(nil, func(page *autoscaling.DescribeLaunchConfigurationsOutput, _ bool) bool {
		for _, lc := range page.LaunchConfigurations {
			l := &launchConfiguration{ID: *lc.LaunchConfigurationARN, Name: *lc.LaunchConfigurationName}
			if set.Mark(l) {
				glog.Warningf("%s: deleting %T: %v", l.ARN(), lc, lc)
				toDelete = append(toDelete, l)
			}
		}
		return true
	}); err != nil {
		return err
	}
	for _, lc := range toDelete {
		_, err := svc.DeleteLaunchConfiguration(
			&autoscaling.DeleteLaunchConfigurationInput{
				LaunchConfigurationName: aws.String(lc.Name),
			})
		if err != nil {
			glog.Warningf("%v: delete failed: %v", lc.ARN(), err)
		}
	}
	return nil
}

type launchConfiguration struct {
	ID   string
	Name string
}

func (lc launchConfiguration) ARN() string {
	return lc.ID
}

// Subnets: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeSubnets

type subnets struct{}

func (subnets) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	resp, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("defaultForAz"),
				Values: []*string{aws.String("false")},
			},
		},
	})
	if err != nil {
		return err
	}

	for _, sub := range resp.Subnets {
		s := &subnet{Account: acct, Region: region, ID: *sub.SubnetId}
		if set.Mark(s) {
			glog.Warningf("%s: deleting %T: %v", s.ARN(), sub, sub)
			_, err := svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: sub.SubnetId})
			if err != nil {
				glog.Warningf("%v: delete failed: %v", s.ARN(), err)
			}
		}
	}
	return nil
}

type subnet struct {
	Account string
	Region  string
	ID      string
}

func (sub subnet) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:subnet/%s", sub.Region, sub.Account, sub.ID)
}

// SecurityGroups: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeSecurityGroups

type securityGroups struct{}

type sgRef struct {
	id   string
	perm *ec2.IpPermission
}

func addRefs(refs map[string][]*sgRef, id string, acct string, perms []*ec2.IpPermission) {
	for _, perm := range perms {
		for _, pair := range perm.UserIdGroupPairs {
			// Ignore cross-account for now, and skip circular refs.
			if *pair.UserId == acct && *pair.GroupId != id {
				refs[*pair.GroupId] = append(refs[*pair.GroupId], &sgRef{id: id, perm: perm})
			}
		}
	}
}

func (securityGroups) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	resp, err := svc.DescribeSecurityGroups(nil)
	if err != nil {
		return err
	}

	var toDelete []*securityGroup        // Deferred to disentangle referencing security groups
	ingress := make(map[string][]*sgRef) // sg.GroupId -> [sg.GroupIds with this ingress]
	egress := make(map[string][]*sgRef)  // sg.GroupId -> [sg.GroupIds with this egress]
	for _, sg := range resp.SecurityGroups {
		if *sg.GroupName == "default" {
			// TODO(zmerlynn): Is there really no better way to detect this?
			continue
		}
		s := &securityGroup{Account: acct, Region: region, ID: *sg.GroupId}
		addRefs(ingress, *sg.GroupId, acct, sg.IpPermissions)
		addRefs(egress, *sg.GroupId, acct, sg.IpPermissionsEgress)
		if set.Mark(s) {
			glog.Warningf("%s: deleting %T: %v", s.ARN(), sg, sg)
			toDelete = append(toDelete, s)
		}
	}
	for _, sg := range toDelete {
		for _, ref := range ingress[sg.ID] {
			glog.Infof("%v: revoking reference from %v", sg.ARN(), ref.id)
			_, err := svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
				GroupId:       aws.String(ref.id),
				IpPermissions: []*ec2.IpPermission{ref.perm},
			})
			if err != nil {
				glog.Warningf("%v: failed to revoke ingress reference from %v: %v", sg.ARN(), ref.id, err)
			}
		}
		for _, ref := range egress[sg.ID] {
			_, err := svc.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
				GroupId:       aws.String(ref.id),
				IpPermissions: []*ec2.IpPermission{ref.perm},
			})
			if err != nil {
				glog.Warningf("%v: failed to revoke egress reference from %v: %v", sg.ARN(), ref.id, err)
			}
		}
		_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(sg.ID)})
		if err != nil {
			glog.Warningf("%v: delete failed: %v", sg.ARN(), err)
		}
	}
	return nil
}

type securityGroup struct {
	Account string
	Region  string
	ID      string
}

func (sg securityGroup) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/%s", sg.Region, sg.Account, sg.ID)
}

// InternetGateways: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeInternetGateways

type internetGateways struct{}

func (internetGateways) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	resp, err := svc.DescribeInternetGateways(nil)
	if err != nil {
		return err
	}

	for _, ig := range resp.InternetGateways {
		i := &internetGateway{Account: acct, Region: region, ID: *ig.InternetGatewayId}
		if set.Mark(i) {
			glog.Warningf("%s: deleting %T: %v", i.ARN(), ig, ig)
			for _, att := range ig.Attachments {
				_, err := svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
					InternetGatewayId: ig.InternetGatewayId,
					VpcId:             att.VpcId,
				})
				if err != nil {
					glog.Warningf("%v: detach from %v failed: %v", i.ARN(), *att.VpcId, err)
				}
			}
			_, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{InternetGatewayId: ig.InternetGatewayId})
			if err != nil {
				glog.Warningf("%v: delete failed: %v", i.ARN(), err)
			}
		}
	}
	return nil
}

type internetGateway struct {
	Account string
	Region  string
	ID      string
}

func (ig internetGateway) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:internet-gateway/%s", ig.Region, ig.Account, ig.ID)
}

// RouteTables: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeRouteTables

type routeTables struct{}

func (routeTables) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	resp, err := svc.DescribeRouteTables(nil)
	if err != nil {
		return err
	}

	for _, rt := range resp.RouteTables {
		// Filter out the RouteTables that have a main
		// association. Given the documention for the main.association
		// filter, you'd think we could filter on the Describe, but it
		// doesn't actually work, see e.g.
		// https://github.com/aws/aws-cli/issues/1810
		main := false
		for _, assoc := range rt.Associations {
			main = main || *assoc.Main
		}
		if main {
			continue
		}
		r := &routeTable{Account: acct, Region: region, ID: *rt.RouteTableId}
		if set.Mark(r) {
			for _, assoc := range rt.Associations {
				glog.Infof("%v: disassociating from %v", r.ARN(), *assoc.SubnetId)
				_, err := svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
					AssociationId: assoc.RouteTableAssociationId})
				if err != nil {
					glog.Warningf("%v: disassociation from subnet %v failed: %v", r.ARN(), *assoc.SubnetId, err)
				}
			}
			glog.Warningf("%s: deleting %T: %v", r.ARN(), rt, rt)
			_, err := svc.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: rt.RouteTableId})
			if err != nil {
				glog.Warningf("%v: delete failed: %v", r.ARN(), err)
			}
		}
	}
	return nil
}

type routeTable struct {
	Account string
	Region  string
	ID      string
}

func (rt routeTable) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:route-table/%s", rt.Region, rt.Account, rt.ID)
}

// VPCs: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeVpcs

type vpcs struct{}

func (vpcs) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	resp, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("isDefault"),
				Values: []*string{aws.String("false")},
			},
		},
	})
	if err != nil {
		return err
	}

	for _, vp := range resp.Vpcs {
		v := &vpc{Account: acct, Region: region, ID: *vp.VpcId}
		if set.Mark(v) {
			glog.Warningf("%s: deleting %T: %v", v.ARN(), vp, vp)
			if *vp.DhcpOptionsId != "default" {
				_, err := svc.AssociateDhcpOptions(&ec2.AssociateDhcpOptionsInput{
					VpcId:         vp.VpcId,
					DhcpOptionsId: aws.String("default"),
				})
				if err != nil {
					glog.Warning("%v: disassociating DHCP option set %v failed: %v", v.ARN(), vp.DhcpOptionsId, err)
				}
			}
			_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: vp.VpcId})
			if err != nil {
				glog.Warningf("%v: delete failed: %v", v.ARN(), err)
			}
		}
	}
	return nil
}

type vpc struct {
	Account string
	Region  string
	ID      string
}

func (vp vpc) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:vpc/%s", vp.Region, vp.Account, vp.ID)
}

// DhcpOptions: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeDhcpOptions

type dhcpOptions struct{}

func (dhcpOptions) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	// This is a little gross, but I can't find an easier way to
	// figure out the DhcpOptions associated with the default VPC.
	defaultRefs := make(map[string]bool)
	{
		resp, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
			Filters: []*ec2.Filter{
				&ec2.Filter{
					Name:   aws.String("isDefault"),
					Values: []*string{aws.String("true")},
				},
			},
		})
		if err != nil {
			return err
		}
		for _, vpc := range resp.Vpcs {
			defaultRefs[*vpc.DhcpOptionsId] = true
		}
	}

	resp, err := svc.DescribeDhcpOptions(nil)
	if err != nil {
		return err
	}

	for _, dhcp := range resp.DhcpOptions {
		if defaultRefs[*dhcp.DhcpOptionsId] {
			continue
		}
		dh := &dhcpOption{Account: acct, Region: region, ID: *dhcp.DhcpOptionsId}
		if set.Mark(dh) {
			glog.Warningf("%s: deleting %T: %v", dh.ARN(), dhcp, dhcp)
			_, err := svc.DeleteDhcpOptions(&ec2.DeleteDhcpOptionsInput{DhcpOptionsId: dhcp.DhcpOptionsId})
			if err != nil {
				glog.Warningf("%v: delete failed: %v", dh.ARN(), err)
			}
		}
	}
	return nil
}

type dhcpOption struct {
	Account string
	Region  string
	ID      string
}

func (dhcp dhcpOption) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:dhcp-option/%s", dhcp.Region, dhcp.Account, dhcp.ID)
}

// Volumes: https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#EC2.DescribeVolumes

type volumes struct{}

func (volumes) MarkAndSweep(sess *session.Session, acct string, region string, set *awsResourceSet) error {
	svc := ec2.New(sess, &aws.Config{Region: aws.String(region)})

	var toDelete []*volume // Paged call, defer deletion until we have the whole list.
	if err := svc.DescribeVolumesPages(nil, func(page *ec2.DescribeVolumesOutput, _ bool) bool {
		for _, vol := range page.Volumes {
			v := &volume{Account: acct, Region: region, ID: *vol.VolumeId}
			if set.Mark(v) {
				glog.Warningf("%s: deleting %T: %v", v.ARN(), vol, vol)
				toDelete = append(toDelete, v)
			}
		}
		return true
	}); err != nil {
		return err
	}
	for _, vol := range toDelete {
		_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(vol.ID)})
		if err != nil {
			glog.Warningf("%v: delete failed: %v", vol.ARN(), err)
		}
	}
	return nil
}

type volume struct {
	Account string
	Region  string
	ID      string
}

func (vol volume) ARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:volume/%s", vol.Region, vol.Account, vol.ID)
}

// ARNs (used for uniquifying within our previous mark file)

type arn struct {
	partition    string
	service      string
	region       string
	account      string
	resourceType string
	resource     string
}

func parseARN(s string) (*arn, error) {
	pieces := strings.Split(s, ":")
	if len(pieces) != 6 || pieces[0] != "arn" || pieces[1] != "aws" {
		return nil, fmt.Errorf("Invalid AWS ARN: %v", s)
	}
	var resourceType string
	var resource string
	res := strings.SplitN(pieces[5], "/", 2)
	if len(res) == 1 {
		resource = res[0]
	} else {
		resourceType = res[0]
		resource = res[1]
	}
	return &arn{
		partition:    pieces[1],
		service:      pieces[2],
		region:       pieces[3],
		account:      pieces[4],
		resourceType: resourceType,
		resource:     resource,
	}, nil
}

func getAccount(sess *session.Session, region string) (string, error) {
	svc := iam.New(sess, &aws.Config{Region: aws.String(region)})
	resp, err := svc.GetUser(nil)
	if err != nil {
		return "", err
	}
	arn, err := parseARN(*resp.User.Arn)
	if err != nil {
		return "", err
	}
	return arn.account, nil
}

type s3path struct {
	region string
	bucket string
	key    string
}

func getS3Path(sess *session.Session, s string) (*s3path, error) {
	url, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if url.Scheme != "s3" {
		return nil, fmt.Errorf("Scheme %q != 's3'", url.Scheme)
	}
	svc := s3.New(sess, &aws.Config{Region: aws.String("us-east-1")})
	resp, err := svc.GetBucketLocation(&s3.GetBucketLocationInput{Bucket: aws.String(url.Host)})
	if err != nil {
		return nil, err
	}
	region := "us-east-1"
	if resp.LocationConstraint != nil {
		region = *resp.LocationConstraint
	}
	return &s3path{region: region, bucket: url.Host, key: url.Path}, nil
}

func main() {
	flag.Lookup("logtostderr").Value.Set("true")
	flag.Parse()
	regions := strings.Split(*regionsRaw, ",")

	// Retry aggressively (with default back-off). If the account is
	// in a really bad state, we may be contending with API rate
	// limiting and fighting against the very resources we're trying
	// to delete.
	sess := session.Must(session.NewSessionWithOptions(session.Options{Config: aws.Config{MaxRetries: aws.Int(100)}}))

	s3p, err := getS3Path(sess, *path)
	if err != nil {
		glog.Fatalf("--path %q isn't a valid S3 path: %v", *path, err)
	}
	acct, err := getAccount(sess, regions[0])
	if err != nil {
		glog.Fatalf("error getting current user: %v", err)
	}
	glog.V(1).Infof("Account: %s", acct)

	res, err := LoadResourceSet(sess, s3p, *maxTTL)
	if err != nil {
		glog.Fatalf("error loading %q: %v", *path, err)
	}
	for _, region := range regions {
		for _, typ := range awsResourceTypes {
			if err := typ.MarkAndSweep(sess, acct, region, res); err != nil {
				glog.Errorf("error sweeping %T: %v", typ, err)
				return
			}
		}
	}
	if err := res.Save(sess, s3p); err != nil {
		glog.Fatalf("error saving %q: %v", *path, err)
	}
}
