package istio

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	"github.com/solo-io/supergloo/pkg/translator/utils"

	"github.com/mitchellh/hashstructure"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	gloov1 "github.com/solo-io/supergloo/pkg/api/external/gloo/v1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/appmesh"

	"github.com/solo-io/solo-kit/pkg/errors"
	"github.com/solo-io/solo-kit/pkg/utils/contextutils"
	"github.com/solo-io/supergloo/pkg/api/v1"
)

// NOTE: copy-pasted from discovery/pkg/fds/discoveries/aws/aws.go
// TODO: aggregate these somewhere
const (
	// expected map identifiers for secrets
	awsAccessKey = "access_key"
	awsSecretKey = "secret_key"
)

// TODO: util method
func NewAwsClientFromSecret(awsSecret *gloov1.Secret_Aws, region string) (*appmesh.AppMesh, error) {
	accessKey := awsSecret.Aws.AccessKey
	if accessKey != "" && !utf8.Valid([]byte(accessKey)) {
		return nil, errors.Errorf("%s not a valid string", awsAccessKey)
	}
	secretKey := awsSecret.Aws.SecretKey
	if secretKey != "" && !utf8.Valid([]byte(secretKey)) {
		return nil, errors.Errorf("%s not a valid string", awsSecretKey)
	}
	sess, err := session.NewSession(aws.NewConfig().
		WithCredentials(credentials.NewStaticCredentials(accessKey, secretKey, "")))
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create AWS session")
	}
	svc := appmesh.New(sess, &aws.Config{Region: aws.String(region)})

	return svc, nil
}

// todo: replace with interface
type AppMeshClient = *appmesh.AppMesh

type AppMeshSyncer struct {
	lock           sync.Mutex
	activeSessions map[uint64]AppMeshClient
}

func NewMeshRoutingSyncer() *AppMeshSyncer {
	return &AppMeshSyncer{
		lock:           sync.Mutex{},
		activeSessions: make(map[uint64]AppMeshClient),
	}
}

func hashCredentials(awsSecret *gloov1.Secret_Aws, region string) uint64 {
	hash, _ := hashstructure.Hash(struct {
		awsSecret *gloov1.Secret_Aws
		region    string
	}{
		awsSecret: awsSecret,
		region:    region,
	}, nil)
	return hash
}

func (s *AppMeshSyncer) NewOrCachedClient(appMesh *v1.AppMesh, secrets gloov1.SecretList) (AppMeshClient, error) {
	secret, err := secrets.Find(appMesh.AwsCredentials.Strings())
	if err != nil {
		return nil, errors.Wrapf(err, "finding aws credentials for mesh")
	}
	region := appMesh.AwsRegion
	if region == "" {
		return nil, errors.Wrapf(err, "mesh must provide aws_region")
	}

	awsSecret, ok := secret.Kind.(*gloov1.Secret_Aws)
	if !ok {
		return nil, errors.Errorf("mesh referenced non-AWS secret, AWS secret required")
	}
	if awsSecret.Aws == nil {
		return nil, errors.Errorf("secret missing field Aws")
	}

	// check if we already have an active session for this region/credential
	sessionKey := hashCredentials(awsSecret, region)
	s.lock.Lock()
	appMeshClient, ok := s.activeSessions[sessionKey]
	s.lock.Unlock()
	if !ok {
		// create a new client and cache it
		// TODO: is there a point where we should drop old sessions?
		// maybe aws will do it for us
		appMeshClient, err = NewAwsClientFromSecret(awsSecret, region)
		if err != nil {
			return nil, errors.Wrapf(err, "creating aws client from provided secret/region")
		}
		s.lock.Lock()
		s.activeSessions[sessionKey] = appMeshClient
		s.lock.Unlock()
	}

	return appMeshClient, nil
}

func (s *AppMeshSyncer) Sync(ctx context.Context, snap *v1.TranslatorSnapshot) error {
	ctx = contextutils.WithLogger(ctx, "mesh-routing-syncer")
	logger := contextutils.LoggerFrom(ctx)
	meshes := snap.Meshes.List()
	upstreams := snap.Upstreams.List()
	rules := snap.Routingrules.List()

	logger.Infof("begin sync %v (%v meshes, %v upstreams, %v rules)", snap.Hash(),
		len(meshes), len(upstreams), len(rules))
	defer logger.Infof("end sync %v", snap.Hash())
	logger.Debugf("%v", snap)

	for _, mesh := range snap.Meshes.List() {
		if err := s.sync(mesh, snap); err != nil {
			return errors.Wrapf(err, "syncing mesh %v", mesh.Metadata.Ref())
		}
	}

	return nil
	/*
		0 - mesh per mesh
		1 - virtual node per upstream
		2 - routing rules get aggregated into virtual service like object
		routes on virtual service become the aws routes
		virtual service becomes virtual router
	*/
	//exampleMesh := appmesh.CreateMeshInput{}
	//exampleVirtualNode := appmesh.CreateVirtualNodeInput{}
	//exampleVirtualRouter := appmesh.CreateVirtualRouterInput{}
	//exampleRoute := appmesh.CreateRouteInput{}
	//
	//destinationRules, err := virtualNodesForUpstreams(rules, meshes, upstreams)
	//if err != nil {
	//	return errors.Wrapf(err, "creating subsets from snapshot")
	//}
	//
	//virtualServices, err := virtualServicesForRules(rules, meshes, upstreams)
	//if err != nil {
	//	return errors.Wrapf(err, "creating virtual services from snapshot")
	//}
	//return s.writeIstioCrds(ctx, destinationRules, virtualServices)
}

func (s *AppMeshSyncer) sync(mesh *v1.Mesh, snap *v1.TranslatorSnapshot) error {
	appMesh, ok := mesh.MeshType.(*v1.Mesh_AppMesh)
	if !ok {
		return nil
	}
	if appMesh.AppMesh == nil {
		return errors.Errorf("%v missing configuration for AppMesh", mesh.Metadata.Ref())
	}
	meshName := fmt.Sprintf("%v.%v", mesh.Metadata.Ref().String())
	desiredMesh := appmesh.CreateMeshInput{
		MeshName: aws.String(meshName),
	}
	upstreams := snap.Upstreams.List()
	virtualNodes, err := virtualNodesFromUpstreams(meshName, upstreams)
	if err != nil {
		return errors.Wrapf(err, "creating virtual nodes from upstreams")
	}
	virtualRouters, err := virtualRoutersFromUpstreams(meshName, upstreams)
	if err != nil {
		return errors.Wrapf(err, "creating virtual routers from upstreams")
	}
	routingRules := snap.Routingrules.List()
	routes, err := routesFromRules(meshName, upstreams, routingRules)
	if err != nil {
		return errors.Wrapf(err, "creating virtual routers from upstreams")
	}

	secrets := snap.Secrets.List()
	client, err := s.NewOrCachedClient(appMesh.AppMesh, secrets)
	if err != nil {
		return errors.Wrapf(err, "creating new AWS AppMesh session")
	}

	if err := resyncState(client, desiredMesh, virtualNodes, virtualRouters, routes); err != nil {
		return errors.Wrapf(err, "reconciling desired state")
	}
	return nil
}

func resyncState(client AppMeshClient,
	mesh appmesh.CreateMeshInput,
	vNodes []appmesh.CreateVirtualNodeInput,
	vRouters []appmesh.CreateVirtualRouterInput,
	routes []appmesh.CreateRouteInput) error {
	if err := reconcileMesh(client, mesh); err != nil {
		return errors.Wrapf(err, "reconciling mesh")
	}

	return nil
}

func reconcileMesh(client AppMeshClient, mesh appmesh.CreateMeshInput) error {
	_, err := client.DescribeMesh(&appmesh.DescribeMeshInput{
		MeshName: mesh.MeshName,
	})
	if err == nil {
		return nil
	}
	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case appmesh.ErrCodeNotFoundException:
			_, err := client.CreateMesh(&mesh)
			return err
		default:
		}
	}
	return errors.Wrapf(err, "failed to check existence of mesh %v", *mesh.MeshName)
}

func reconcileVirtualNodes(client AppMeshClient, mesh appmesh.CreateMeshInput, vNodes []appmesh.CreateVirtualNodeInput) error {
	existingVirtualNodes, err := client.ListVirtualNodes(&appmesh.ListVirtualNodesInput{
		MeshName: mesh.MeshName,
	})
	if err != nil {
		return errors.Wrapf(err, "failed to list existing virtual nodes for mesh %v", *mesh.MeshName)
	}
	for _, desiredVNode := range vNodes {
		if err := reconcileVirtualNode(client, desiredVNode, existingVirtualNodes.VirtualNodes); err != nil {
			return errors.Wrapf(err, "reconciling virtual node %v", *desiredVNode.VirtualNodeName)
		}
	}
	// delete unused
	for _, original := range existingVirtualNodes.VirtualNodes {
		var unused bool
		for _, desired := range vNodes {

		}
	}
	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case appmesh.ErrCodeNotFoundException:
			_, err := client.CreateMesh(&mesh)
			return err
		default:
		}
	}

}

func reconcileVirtualNode(client AppMeshClient, desiredVNode appmesh.CreateVirtualNodeInput, existingVirtualNodes []*appmesh.VirtualNodeRef) error {
	for _, node := range existingVirtualNodes {
		if aws.StringValue(node.VirtualNodeName) == *desiredVNode.VirtualNodeName {
			// update
			originalVNode, err := client.DescribeVirtualNode(&appmesh.DescribeVirtualNodeInput{
				MeshName:        desiredVNode.MeshName,
				VirtualNodeName: desiredVNode.VirtualNodeName,
			})
			if err != nil {
				return errors.Wrapf(err, "retrieving original node for update")
			}
			// TODO: find a better way of comparing AWS structs
			if originalVNode.VirtualNode.Spec.String() == desiredVNode.Spec.String() {
				// spec already matches, nothing to do
				return nil
			}
			if _, err := client.UpdateVirtualNode(&appmesh.UpdateVirtualNodeInput{
				MeshName:        desiredVNode.MeshName,
				VirtualNodeName: desiredVNode.VirtualNodeName,
				Spec:            desiredVNode.Spec,
			}); err != nil {
				return errors.Wrapf(err, "updating virtual node")
			}
		}
	}
	if _, err := client.CreateVirtualNode(&desiredVNode); err != nil {
		return errors.Wrapf(err, "creating virtual node")
	}

	return nil
}

func virtualNodesFromUpstreams(meshName string, upstreams gloov1.UpstreamList) ([]appmesh.CreateVirtualNodeInput, error) {
	portsByHost := make(map[string][]uint32)
	// TODO: filter hosts by policy, i.e. only what the user wants
	var allHosts []string
	for _, us := range upstreams {
		host, err := utils.GetHostForUpstream(us)
		if err != nil {
			return nil, errors.Wrapf(err, "getting host for upstream")
		}
		port, err := utils.GetPortForUpstream(us)
		if err != nil {
			return nil, errors.Wrapf(err, "getting port for upstream")
		}
		portsByHost[host] = append(portsByHost[host], port)
		allHosts = append(allHosts, host)
	}

	var virtualNodes []appmesh.CreateVirtualNodeInput
	for host, ports := range portsByHost {
		var listeners []*appmesh.Listener
		for _, port := range ports {
			listener := &appmesh.Listener{
				PortMapping: &appmesh.PortMapping{
					// TODO: support more than just http here
					Protocol: aws.String("http"),
					Port:     aws.Int64(int64(port)),
				},
			}
			listeners = append(listeners, listener)
		}
		virtualNode := appmesh.CreateVirtualNodeInput{
			MeshName: aws.String(meshName),
			Spec: &appmesh.VirtualNodeSpec{
				Backends:  aws.StringSlice(allHosts),
				Listeners: listeners,
				ServiceDiscovery: &appmesh.ServiceDiscovery{
					Dns: &appmesh.DnsServiceDiscovery{
						ServiceName: aws.String(host),
					},
				},
			},
		}
		virtualNodes = append(virtualNodes, virtualNode)
	}
	sort.SliceStable(virtualNodes, func(i, j int) bool {
		return virtualNodes[i].String() < virtualNodes[j].String()
	})
	return virtualNodes, nil
}

func virtualRoutersFromUpstreams(meshName string, upstreams gloov1.UpstreamList) ([]appmesh.CreateVirtualRouterInput, error) {
	var allHosts []string
	for _, us := range upstreams {
		host, err := utils.GetHostForUpstream(us)
		if err != nil {
			return nil, errors.Wrapf(err, "getting host for upstream")
		}
		allHosts = append(allHosts, host)
	}

	var virtualNodes []appmesh.CreateVirtualRouterInput
	for _, host := range allHosts {
		virtualNode := appmesh.CreateVirtualRouterInput{
			MeshName: aws.String(meshName),
			Spec: &appmesh.VirtualRouterSpec{
				ServiceNames: aws.StringSlice([]string{host}),
			},
		}
		virtualNodes = append(virtualNodes, virtualNode)
	}
	sort.SliceStable(virtualNodes, func(i, j int) bool {
		return virtualNodes[i].String() < virtualNodes[j].String()
	})
	return virtualNodes, nil
}

func routesFromRules(meshName string, upstreams gloov1.UpstreamList, routingRules v1.RoutingRuleList) ([]appmesh.CreateRouteInput, error) {
	// todo: using selector, figure out which source upstreams and which destinations need
	// the route. we are only going to support traffic shifting for now
	var allHosts []string
	for _, us := range upstreams {
		host, err := utils.GetHostForUpstream(us)
		if err != nil {
			return nil, errors.Wrapf(err, "getting host for upstream")
		}
		allHosts = append(allHosts, host)
	}

	var routes []appmesh.CreateRouteInput

	for _, rule := range routingRules {
		if rule.TrafficShifting == nil {
			// only traffic shifting is currently supported on AppMesh
			continue
		}

		// NOTE: sources get ignored. AppMesh applies rules to all sources in the mesh
		var destinationHosts []string

		for _, usRef := range rule.Destinations {
			us, err := upstreams.Find(usRef.Strings())
			if err != nil {
				return nil, errors.Wrapf(err, "cannot find destination for routing rule")
			}
			host, err := utils.GetHostForUpstream(us)
			if err != nil {
				return nil, errors.Wrapf(err, "getting host for upstream")
			}
			var alreadyAdded bool
			for _, added := range destinationHosts {
				if added == host {
					alreadyAdded = true
					break
				}
			}
			if !alreadyAdded {
				destinationHosts = append(destinationHosts, host)
			}
		}

		var targets []*appmesh.WeightedTarget
		// NOTE: only 3 destinations are allowed at time of release
		for _, dest := range rule.TrafficShifting.Destinations {
			us, err := upstreams.Find(dest.Upstream.Strings())
			if err != nil {
				return nil, errors.Wrapf(err, "cannot find destination for routing rule")
			}
			destinationHost, err := utils.GetHostForUpstream(us)
			if err != nil {
				return nil, errors.Wrapf(err, "getting host for destination upstream")
			}
			targets = append(targets, &appmesh.WeightedTarget{
				VirtualNode: aws.String(destinationHost),
				Weight:      aws.Int64(int64(dest.Weight)),
			})
		}

		prefix := "/"
		if len(rule.RequestMatchers) > 0 {
			// TODO: when appmesh supports multiple matchers, we should too
			// for now, just pick the first one
			match := rule.RequestMatchers[0]
			// TODO: when appmesh supports more types of path matching, we should too
			if prefixSpecifier, ok := match.PathSpecifier.(*gloov1.Matcher_Prefix); ok {
				prefix = prefixSpecifier.Prefix
			}
		}
		for _, host := range destinationHosts {
			route := appmesh.CreateRouteInput{
				MeshName:          aws.String(meshName),
				RouteName:         aws.String(host + "-" + rule.Metadata.Namespace + "-" + rule.Metadata.Name),
				VirtualRouterName: aws.String(host),
				Spec: &appmesh.RouteSpec{
					HttpRoute: &appmesh.HttpRoute{
						Match: &appmesh.HttpRouteMatch{
							Prefix: aws.String(prefix),
						},
						Action: &appmesh.HttpRouteAction{
							WeightedTargets: targets,
						},
					},
				},
			}
			routes = append(routes, route)
		}
	}

	sort.SliceStable(routes, func(i, j int) bool {
		return routes[i].String() < routes[j].String()
	})
	return routes, nil
}

func appMeshExampleMeshAndSecret() (*v1.Mesh, *gloov1.Secret) {
	secret := &gloov1.Secret{
		Metadata: core.Metadata{Name: "my-aws-credentials", Namespace: "my-namespace"},
		Kind: &gloov1.Secret_Aws{
			Aws: &gloov1.AwsSecret{
				// these can be read in from ~/.aws/credentials by default (if user does not provide)
				// see https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html for more details
				AccessKey: "MY-ACCESS-KEY",
				SecretKey: "MY-SECRET-KEY",
			},
		},
	}
	ref := secret.Metadata.Ref()
	mesh := &v1.Mesh{
		Metadata: core.Metadata{Name: "my-mesh", Namespace: "my-namespace"},
		MeshType: &v1.Mesh_AppMesh{
			AppMesh: &v1.AppMesh{
				AwsRegion:      "us-east-1",
				AwsCredentials: &ref,
			},
		},
	}
	return mesh, secret
}