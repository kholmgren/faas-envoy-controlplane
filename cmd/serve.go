package cmd

import (
	"context"
	"fmt"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoy_extensions_filters_http_ext_authz_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	runtimeservice "github.com/envoyproxy/go-control-plane/envoy/service/runtime/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/golang/protobuf/ptypes/duration"

	"github.com/golang/protobuf/ptypes"
	"github.com/kholmgren/faas-envoy-controlplane/internal"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	testv3 "github.com/envoyproxy/go-control-plane/pkg/test/v3"
)

var (
	manifestFile string
	l            internal.Logger
	port         uint
	nodeID       string
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,

	Args: cobra.MaximumNArgs(1),

	Run: func(cmd *cobra.Command, args []string) {
		serve(cmd, args)
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)

	l = internal.Logger{}

	serveCmd.PersistentFlags().BoolVarP(
		&l.Debug, "debug", "", false, "Enable xDS server debug logging")

	serveCmd.PersistentFlags().UintVarP(
		&port, "port", "", 9000, "xDS management server port")

	serveCmd.PersistentFlags().StringVarP(
		&nodeID, "nodeID", "", "faas", "Node ID")
}

const (
	grpcMaxConcurrentStreams = 1000000
)

func serve(cmd *cobra.Command, args []string) {
	manifestFile := args[0]

	_, err := os.Stat(manifestFile)
	if os.IsNotExist(err) {
		fmt.Println("manifest file does not exist")
	}

	manifestBytes, err := ioutil.ReadFile(manifestFile)
	if err != nil {
		l.Errorf("error: %v", err)
	}

	manifest := internal.Manifest{}
	err = yaml.Unmarshal(manifestBytes, &manifest)
	if err != nil {
		l.Errorf("error: %v", err)
	}

	l.Infof("--- # manifest\n%v\n\n", string(manifestBytes))

	// Create a cache
	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, l)

	// Create the snapshot that we'll serve to Envoy
	snapshot := generateSnapshot(manifest)
	if err := snapshot.Consistent(); err != nil {
		l.Errorf("snapshot inconsistency: %+v\n%+v", snapshot, err)
		os.Exit(1)
	}

	dump, err := yaml.Marshal(&snapshot)
	if err != nil {
		l.Errorf("error: %v", err)
	}
	l.Infof("--- # snapshot\n%s\n\n", string(dump))

	// Add the snapshot to the cache
	if err := snapshotCache.SetSnapshot(nodeID, snapshot); err != nil {
		l.Errorf("snapshot error %q for %+v", err, snapshot)
		os.Exit(1)
	}

	// Run the xDS server
	ctx := context.Background()
	cb := &testv3.Callbacks{Debug: l.Debug}
	srv := serverv3.NewServer(ctx, snapshotCache, cb)

	log.Printf("\nxDS server listening on port %d\n\n", port)

	runServer(srv, port)
	if err != nil {
		l.Errorf(err.Error())
		panic(err)
	}
}

func runServer(server serverv3.Server, port uint) (err error) {
	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams))
	grpcServer := grpc.NewServer(grpcOptions...)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return
	}

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, server)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, server)
	runtimeservice.RegisterRuntimeDiscoveryServiceServer(grpcServer, server)

	err = grpcServer.Serve(lis)

	return
}

func generateSnapshot(manifest internal.Manifest) cache.Snapshot {
	var routes []*route.Route

	routes = append(routes, makeRoute(true, "/acl/", "acl_api", map[string]string{
		"namespace_object":  "acl",
		"namespace_service": "api",
		"service_path":      "/acl/{objectId}",
		"relation":          "owner",
	}))

	for path, pathManifest := range manifest.Paths {
		materialized := make(map[string]string)
		for k, v := range manifest.Authorization.Extensions {
			materialized[k] = v
		}

		materialized["service_path"] = path

		if pathManifest.Authorization.ObjectIdPtr != "" {
			materialized["objectid_ptr"] = path
		}

		for k, v := range pathManifest.Authorization.Extensions {
			materialized[k] = v
		}

		routes = append(routes, makeRoute(false, path, "invoker", materialized))
	}

	return cache.NewSnapshot(
		"1",
		[]types.Resource{}, // endpoints
		[]types.Resource{
			makeCluster("invoker", "invoker", 8080),
			makeCluster("acl_api", "authz", 8081),
			makeCluster("authz", "authz", 8080),
		}, // clusters
		[]types.Resource{},                     // routes
		[]types.Resource{makeListener(routes)}, // listeners
		[]types.Resource{},                     // runtimes
		[]types.Resource{},                     // secrets
	)
}

func makeCluster(name string, host string, port uint32) *cluster.Cluster {
	return &cluster.Cluster{
		Name:                 name,
		ConnectTimeout:       ptypes.DurationProto(1 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STRICT_DNS},
		LbPolicy:             cluster.Cluster_ROUND_ROBIN,
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpoint.LocalityLbEndpoints{{
				LbEndpoints: []*endpoint.LbEndpoint{{
					HostIdentifier: &endpoint.LbEndpoint_Endpoint{
						Endpoint: &endpoint.Endpoint{
							Address: &core.Address{
								Address: &core.Address_SocketAddress{
									SocketAddress: &core.SocketAddress{
										Protocol: core.SocketAddress_TCP,
										Address:  host,
										PortSpecifier: &core.SocketAddress_PortValue{
											PortValue: port,
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
	}
}

func makeListener(routes []*route.Route) *listener.Listener {
	extAuthAny, err := ptypes.MarshalAny(
		&envoy_extensions_filters_http_ext_authz_v3.ExtAuthz{
			TransportApiVersion:    2, //3, zero-based enum
			IncludePeerCertificate: true,
			WithRequestBody: &envoy_extensions_filters_http_ext_authz_v3.BufferSettings{
				MaxRequestBytes:     65536,
				AllowPartialMessage: false,
				PackAsBytes:         false,
			},

			Services: &envoy_extensions_filters_http_ext_authz_v3.ExtAuthz_GrpcService{
				GrpcService: &core.GrpcService{
					TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: "xds"},
					},
					Timeout: &duration.Duration{Seconds: 1},
				},
			},
		})

	if err != nil {
		panic(err)
	}

	managerAny, err := ptypes.MarshalAny(&hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: "ingress_http",

		HttpFilters: []*hcm.HttpFilter{
			{
				Name:       "envoy.ext_authz1",
				ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: extAuthAny},
			},
		},

		RouteSpecifier: &hcm.HttpConnectionManager_RouteConfig{
			RouteConfig: &route.RouteConfiguration{

				Name: "local_route",
				VirtualHosts: []*route.VirtualHost{
					{
						Name:    "backend",
						Domains: []string{"*"},
						Routes:  routes,
					},
				},
			},
		},
	})

	if err != nil {
		panic(err)
	}

	return &listener.Listener{
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol:      core.SocketAddress_TCP,
					Address:       "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{PortValue: 18000},
				},
			},
		},

		FilterChains: []*listener.FilterChain{{
			Filters: []*listener.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listener.Filter_TypedConfig{TypedConfig: managerAny},
			}},
		}},
	}
}

func makeRoute(prefix bool, path string, cluster string, materializedExtensions map[string]string) *route.Route {
	var m route.RouteMatch

	if prefix {
		m = route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: path}}
	} else {
		m = route.RouteMatch{PathSpecifier: &route.RouteMatch_Path{Path: path}}
	}

	extAuthzPerRouteAny, err := ptypes.MarshalAny(&envoy_extensions_filters_http_ext_authz_v3.ExtAuthzPerRoute{
		Override: &envoy_extensions_filters_http_ext_authz_v3.ExtAuthzPerRoute_CheckSettings{
			CheckSettings: &envoy_extensions_filters_http_ext_authz_v3.CheckSettings{
				ContextExtensions: materializedExtensions,
			},
		},
	})

	if err != nil {
		panic(err)
	}

	return &route.Route{
		Match: &m,
		Action: &route.Route_Route{
			Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{Cluster: cluster},
			},
		},
		TypedPerFilterConfig: map[string]*any.Any{"envoy.filters.http.ext_authz": extAuthzPerRouteAny},
	}
}
