package profile

import (
	"encoding/json"
	"fmt"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"

	// "io/ioutil"
	"net"
	// "os"
	"sync"
	"time"

	"github.com/bjornleffler/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/harlow/go-micro-services/registry"
	pb "github.com/harlow/go-micro-services/services/profile/proto"
	"github.com/harlow/go-micro-services/tls"
	"github.com/opentracing/opentracing-go"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/bradfitz/gomemcache/memcache"
	// "strings"
)

const name = "srv-profile"

// Server implements the profile service
type Server struct {
	Tracer         opentracing.Tracer
	uuid           string
	Port           int
	PrometheusPort int
	IpAddr         string
	MongoSession   *mgo.Session
	Registry       *registry.Client
	MemcClient     *memcache.Client
}

var (
	clientRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "client_request_total",
		Help: "The total number of client requests, by service and method."},
		[]string{"service", "method"})
	clientLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "client_latency",
		Help: "Client request latency, by service and method."},
		[]string{"service", "method"})

	serverRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "server_request_total",
		Help: "The total number of client requests, by method."},
		[]string{"method"})
	serverLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "server_latency",
		Help: "Server request latency, by method."},
		[]string{"method"})

	// "Exclusive" latency is the strict server latency, excluding client latency.
	serverExclusiveLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "server_exclusive_latency",
		Help: "Server exclusive request latency, by method."},
		[]string{"method"})
)

// Run starts the server
func (s *Server) Run() error {
	opentracing.SetGlobalTracer(s.Tracer)

	if s.Port == 0 {
		return fmt.Errorf("server port must be set")
	}

	s.uuid = uuid.New().String()

	log.Trace().Msgf("in run s.IpAddr = %s, port = %d", s.IpAddr, s.Port)

	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Timeout: 120 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			PermitWithoutStream: true,
		}),
		grpc.UnaryInterceptor(
			otgrpc.OpenTracingServerInterceptor(s.Tracer),
		),
	}

	if tlsopt := tls.GetServerOpt(); tlsopt != nil {
		opts = append(opts, tlsopt)
	}

	srv := grpc.NewServer(opts...)

	pb.RegisterProfileServer(srv, s)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Port))
	if err != nil {
		log.Fatal().Msgf("failed to configure listener: %v", err)
	}

	// Configure Prometheus exports and tracing.
	tracing.Configure("profile", s.PrometheusPort)

	// register the service
	// jsonFile, err := os.Open("config.json")
	// if err != nil {
	// 	fmt.Println(err)
	// }

	// defer jsonFile.Close()

	// byteValue, _ := ioutil.ReadAll(jsonFile)

	// var result map[string]string
	// json.Unmarshal([]byte(byteValue), &result)

	err = s.Registry.Register(name, s.uuid, s.IpAddr, s.Port)
	if err != nil {
		return fmt.Errorf("failed register: %v", err)
	}
	log.Info().Msg("Successfully registered in consul")

	return srv.Serve(lis)
}

// Shutdown cleans up any processes
func (s *Server) Shutdown() {
	s.Registry.Deregister(s.uuid)
}

// GetProfiles returns hotel profiles for requested IDs
func (s *Server) GetProfiles(ctx context.Context, req *pb.Request) (*pb.Result, error) {
	serverSpan := tracing.StartServerSpan(ctx, "GetProfiles")
	defer serverSpan.Finish()

	// session, err := mgo.Dial("mongodb-profile")
	// if err != nil {
	// 	panic(err)
	// }
	// defer session.Close()

	log.Trace().Msgf("In GetProfiles")

	res := new(pb.Result)
	hotels := make([]*pb.Hotel, 0)
	var wg sync.WaitGroup
	var mutex sync.Mutex

	// one hotel should only have one profile
	hotelIds := make([]string, 0)
	profileMap := make(map[string]struct{})
	for _, hotelId := range req.HotelIds {
		hotelIds = append(hotelIds, hotelId)
		profileMap[hotelId] = struct{}{}
	}
	clientSpan := tracing.StartClientSpan(ctx, serverSpan, "memcached", "get_profile")
	resMap, err := s.MemcClient.GetMulti(hotelIds)
	clientSpan.Finish()
	if err != nil && err != memcache.ErrCacheMiss {
		log.Panic().Msgf("Tried to get hotelIds [%v], but got memmcached error = %s", hotelIds, err)
	} else {
		for hotelId, item := range resMap {
			profileStr := string(item.Value)
			log.Trace().Msgf("memc hit with %v", profileStr)

			hotelProf := new(pb.Hotel)
			json.Unmarshal(item.Value, hotelProf)
			hotels = append(hotels, hotelProf)
			delete(profileMap, hotelId)
		}

		wg.Add(len(profileMap))
		for hotelId := range profileMap {
			go func(hotelId string) {
				session := s.MongoSession.Copy()
				defer session.Close()
				c := session.DB("profile-db").C("hotels")

				hotelProf := new(pb.Hotel)
				clientSpan := tracing.StartClientSpan(ctx, serverSpan, "mongo", "profile")
				err := c.Find(bson.M{"id": hotelId}).One(&hotelProf)
				clientSpan.Finish()

				if err != nil {
					log.Error().Msgf("Failed get hotels data: ", err)
				}

				mutex.Lock()
				hotels = append(hotels, hotelProf)
				mutex.Unlock()

				profJson, err := json.Marshal(hotelProf)
				if err != nil {
					log.Error().Msgf("Failed to marshal hotel [id: %v] with err:", hotelProf.Id, err)
				}
				memcStr := string(profJson)

				// write to memcached
				go s.MemcClient.Set(&memcache.Item{Key: hotelId, Value: []byte(memcStr)})
				defer wg.Done()
			}(hotelId)
		}
	}
	wg.Wait()

	res.Hotels = hotels
	log.Trace().Msgf("In GetProfiles after getting resp")
	return res, nil
}
