package morpheus

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/madflojo/tasks"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

const DefaultTTL = 5 * time.Second

const DefaultHBInterval = 2 * time.Second

type MessageHandler func(m *Morpheus, msg *Message)

type Message struct {
	Timestamp       int64       `json:"timestamp,omitempty"`
	MsgId           string      `json:"msg_id,omitempty"`
	ResponseChannel string      `json:"response_channel,omitempty"`
	Channel         string      `json:"channel,omitempty"`
	Route           string      `json:"route,omitempty"`
	From            string      `json:"from,omitempty"`
	To              string      `json:"to,omitempty"`
	Payload         interface{} `json:"payload,omitempty"`
}

func (m Message) Json() []byte {
	b, err := json.Marshal(m)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal message")
	}
	return b
}

type Morpheus struct {
	client    *redis.Client
	context   context.Context
	Services  Services // map[service_name]map[service_id]*Service
	Scheduler *tasks.Scheduler
}

type Services map[string]map[string]*Service

func (s Services) Add(svc *Service) {
	if s[svc.Name] == nil {
		s[svc.Name] = make(map[string]*Service)
	}
	s[svc.Name][svc.Id] = svc
}

func (s Services) Remove(svc Service) {
	close(svc.LivenessChannel)
	delete(s[svc.Name], svc.Id)
} // map[service_name]map[service_id]*Service

type Service struct {
	Id              string        `json:"id,omitempty"`
	Name            string        `json:"name,omitempty"`
	IpAddress       string        `json:"ip_address,omitempty"`
	Port            int           `json:"port,omitempty"`
	Routes          ServiceRoutes `json:"routes,omitempty"`
	LivenessChannel chan bool     `json:"-"`
}
type ServiceRoutes []ServiceRoute

func (s ServiceRoutes) Len() int {
	return len(s)
}

func (s ServiceRoutes) Less(i, j int) bool {
	return len(s[i].Route) > len(s[j].Route)
}

type ServiceRoute struct {
	Route   string
	Handler MessageHandler `json:"-"` // used to handle messages
}

func (s Service) Key() string {
	return fmt.Sprintf("morpheus:service:%s:%s", s.Name, s.Id)
}

func (s Service) getBaseKey() string {
	return fmt.Sprintf("morpheus:service:%s", s.Name)
}

func (s Service) Match(path string) bool {
	for _, route := range s.Routes {
		if strings.HasPrefix(path, route.Route) {
			return true
		}
	}
	return false
}

func (m *Morpheus) Connect() error {
	host, ok := os.LookupEnv("REDIS_HOST")
	if !ok {
		host = "localhost:6379"
	}

	clientName := randomId()

	username := ""
	password := ""
	if un, ok := os.LookupEnv("REDIS_USERNAME"); ok {
		if pw, ok := os.LookupEnv("REDIS_PASSWORD"); ok {
			username = un
			password = pw
		}
	}

	m.client = redis.NewClient(&redis.Options{
		Addr:       host,
		ClientName: clientName,
		Username:   username,
		Password:   password,
		DB:         0,
	})
	if m.context == nil {
		m.context = context.Background()
	}
	return nil
}
func (m *Morpheus) RegisterService(name string, port int, routes ServiceRoutes, handler func(m *Morpheus, msg *Message)) (*Service, error) {
	svcId := randomId()
	if m.serviceExists(name, svcId) {
		return nil, fmt.Errorf("service already exists")
	}
	svc := &Service{
		Id:        svcId,
		Name:      name,
		IpAddress: m.getIpAddress(),
		Port:      port,
		Routes:    routes,
	}
	m.Services.Add(svc)
	m.UpdateService(svc)
	channels := []string{svc.Key(), svc.getBaseKey()}
	if handler != nil {
		go func() {
			sub := m.client.Subscribe(m.context, channels...)
			log.Trace().Strs("channels", channels).Msg("subscribed to channels")
			for {
				select {
				case msg := <-sub.Channel():
					var message Message
					err := json.Unmarshal([]byte(msg.Payload), &message)
					if err != nil {
						log.Error().Err(err).Msg("failed to unmarshal message")
						continue
					}
					handler(m, &message)
				case <-svc.LivenessChannel:
					log.Warn().Str("id", svc.Id).Msg("service liveness channel closed")
					return
				}
			}
		}()
	}
	err := m.Scheduler.AddWithID(svc.Id, &tasks.Task{
		Interval: DefaultHBInterval,
		TaskFunc: func() error {
			log.Trace().Str("id", svc.Id).Msg("updating service")
			m.UpdateService(svc)
			return nil
		},
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to add task")
	}
	return svc, nil
}
func (m *Morpheus) internalListServices() int {
	return len(m.Services)
}
func (m *Morpheus) ListServices() []Service {
	keys := m.client.Keys(m.context, "morpheus:service:*:presence")
	if keys.Err() != nil {
		log.Error().Err(keys.Err()).Msg("failed to list services")
		return make([]Service, 0)
	}
	if len(keys.Val()) > 0 {
		svcs := make([]Service, 0)
		for _, key := range keys.Val() {
			val := m.client.Get(m.context, key)
			if val.Err() != nil {
				log.Error().Err(val.Err()).Msg("failed to get service")
				continue
			}
			svcName := getServiceName(key)
			svc, err := m.FetchService(svcName, val.Val())
			if err != nil {
				log.Error().Err(err).Msg("failed to fetch service")
				continue
			}
			svcs = append(svcs, svc)
		}
		sort.SliceStable(svcs, func(i, j int) bool {
			return svcs[i].Id < svcs[j].Id
		})
		return svcs
	}
	return make([]Service, 0)
}

func (m *Morpheus) DeleteService(svc Service) {
	delete(m.Services, svc.Id)
}
func (m *Morpheus) serviceExists(name string, id string) bool {
	return m.Services[name][id] != nil
}

func (m *Morpheus) UpdatePresence(svc *Service) {
	key := fmt.Sprintf("%s:presence", svc.Key())
	m.client.Set(m.context, key, svc.Id, DefaultTTL)
}

func (m *Morpheus) UpdateService(svc *Service) {
	m.UpdatePresence(svc)
	m.UpdateHealth(svc)
	m.UpdateRoutes(svc)
}

func (m *Morpheus) UpdateHealth(svc *Service) {
	key := fmt.Sprintf("%s:health", svc.Key())
	jSvc, err := json.Marshal(svc)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal service")
		return
	}
	m.client.Set(m.context, key, string(jSvc), DefaultTTL)
}

func (m *Morpheus) UpdateRoutes(svc *Service) {
	key := fmt.Sprintf("%s:routes", svc.Key())
	routes := make([]string, 0)
	for _, route := range svc.Routes {
		routes = append(routes, route.Route)
	}
	m.client.SAdd(m.context, key, routes)
	m.client.Expire(m.context, key, DefaultTTL)
}

func (m *Morpheus) getIpAddress() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatal().Err(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func (m *Morpheus) FlushDB() {
	_, err := m.client.FlushDB(m.context).Result()
	if err != nil {
		log.Error().Err(err).Msg("failed to flush db")
	}
}
func (m *Morpheus) RPC(from string, service Service, route string, payload interface{}) chan *Message {
	retch := make(chan *Message)
	m.Message("rpc", from, service.Key(), route, payload, nil, retch)
	return retch
}

func (m *Morpheus) RPCWithTimeout(from string, service Service, route string, payload interface{}, timeout time.Duration) chan *Message {
	retch := make(chan *Message)
	m.Message(service.Key(), from, service.Key(), route, payload, nil, retch)
	go func() {
		time.Sleep(timeout)
		retch <- nil
	}()
	return retch
}

func (m *Morpheus) Respond(msg *Message, payload interface{}) {
	m.Message(msg.ResponseChannel, msg.To, msg.From, msg.Route, payload, &msg.ResponseChannel, nil)
}
func (m *Morpheus) Message(channel, from, to, route string, payload interface{}, responseTo *string, retch chan *Message) {
	msgId := randomId()
	var message = Message{
		Timestamp: time.Now().Unix(),
		From:      from,
		To:        to,
		Payload:   payload,
		Channel:   channel,
		MsgId:     msgId,
		Route:     route,
	}
	if responseTo == nil {
		message.ResponseChannel = fmt.Sprintf("%s:response:%s", channel, message.MsgId)
	}
	jMsg, err := json.Marshal(message)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal message")
		return
	}
	if responseTo == nil {
		sub := m.client.Subscribe(m.context, message.ResponseChannel)
		go func() {
			msg, err := sub.ReceiveMessage(m.context)
			if err != nil {
				log.Error().Err(err).Msg("failed to receive message")
				return
			}
			var response Message
			err = json.Unmarshal([]byte(msg.Payload), &response)
			if err != nil {
				log.Error().Err(err).Msg("failed to unmarshal response")
				return
			}
			retch <- &response
			_ = sub.Close()
		}()
	}
	m.client.Publish(m.context, channel, string(jMsg))
}

func (m *Morpheus) FetchService(name string, id string) (Service, error) {
	key := fmt.Sprintf("morpheus:service:%s:%s:health", name, id)
	val := m.client.Get(m.context, key)
	if val.Err() != nil {
		return Service{}, val.Err()
	}
	var svc Service
	err := json.Unmarshal([]byte(val.Val()), &svc)
	if err != nil {
		return Service{}, err
	}
	return svc, nil
}

func (m *Morpheus) ResolveService(path string) (*Service, error) {
	out := make([]Service, 0)
	for _, svc := range m.ListServices() {
		if svc.Match(path) {
			out = append(out, svc)
		}
	}
	if len(out) > 0 {
		idx := rand.Int() % len(out)
		return &out[idx], nil
	}
	return nil, fmt.Errorf("service not found")
}

func getBaseKey(key string) string {
	parts := strings.Split(key, ":")
	return strings.Join(parts[:3], ":")
}
func getServiceName(key string) string {
	parts := strings.Split(key, ":")
	return parts[2]
}

func Init() (*Morpheus, error) {
	m := Morpheus{
		Services:  make(Services),
		Scheduler: tasks.New(),
	}
	err := m.Connect()
	if err != nil {
		return nil, err
	}
	return &m, nil
}
