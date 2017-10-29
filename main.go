package main

import (
	"log"
	"net"
	"strconv"

	"os"
	"os/signal"
	"syscall"

	"sync"

	"math/rand"

	"time"

	_"bytes"

	"bytes"

	"fmt"

	"reflect"

	"github.com/google/uuid"
	c "github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/watch"
	_ "github.com/hashicorp/consul/watch"
	"github.com/zond/gotomic"
)

type Connection struct {
	scheme, addr string
	port         int
}

func (c *Connection) String() string {
	return c.addr + ":" + strconv.Itoa(c.port)
}

var consulConn = &Connection{
	"",
	"consul",
	8500,
}

type Chatter struct {
	id          string
	conn        Connection
	listener    net.Listener
	consul      *c.Client
	state       int
	shutdown    chan bool
	lock        *sync.RWMutex
	connections *gotomic.Hash
	plan        *watch.Plan
}

const (
	STATE_START    = iota
	STATE_STARTING
	STATE_RUN
	STATE_STOPPING
	STATE_STOP
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())
	port := 6000 + rand.Int31n(999)

	server := &Chatter{
		uuid.New().String(),
		Connection{
			"tcp",
			"localhost",
			int(port),
		},
		nil,
		nil,
		STATE_START,
		make(chan bool),
		&sync.RWMutex{},
		gotomic.NewHash(),
		nil,
	}

	log.Printf("starting chatter (%v)\n", server.conn.String())
	server.buildSignalHandler()
	server.buildConsul()
	server.buildListener()
	server.register()

	server.startSearchingForPeers()
	server.awaitShutdown()
}

func (server *Chatter) register() {
	err := server.consul.Agent().ServiceRegister(&c.AgentServiceRegistration{
		ID:      server.id,
		Name:    "chatter",
		Port:    server.conn.port,
		Address: server.conn.addr,
	})
	panicOnError(err)
	log.Printf("registerd: %v\n", server.id)
}

func (server *Chatter) buildConsul() {
	configConsul := buildConsulConfig(consulConn.String())
	consul, err := c.NewClient(configConsul)
	panicOnError(err)
	server.consul = consul
}

func (server *Chatter) buildListener() {
	listener, err := net.Listen(server.conn.scheme, server.conn.String())
	panicOnError(err)
	log.Printf("chatter listening on (%v)\n", listener.Addr().String())

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("%s failed to accept %s: %s\n",
					server.conn.String(),
					conn.RemoteAddr(),
					err.Error())
				continue
			}
			addrKey := gotomic.StringKey(conn.RemoteAddr().String())
			log.Printf("%s accepted: %s\n", server.conn.String(), conn.RemoteAddr())
			log.Printf("%s greeting: %s\n", server.conn.String(), conn.RemoteAddr())

			w, err := conn.Write([]byte("yo"))
			response := make([]byte, 4)
			r, err := conn.Read(response)

			log.Printf("%s received: %s (r %d w %d) \"%s\"\n",
				server.conn,
				conn.RemoteAddr(),
				r, w,
				response)

			log.Printf("%s added: %s\n", server.conn, conn.RemoteAddr())
			server.connections.Put(addrKey, conn)
		}
	}()

	server.listener = listener
}

func (server *Chatter) buildSignalHandler() {
	signalCh := make(chan os.Signal)
	signal.Notify(signalCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for {
			s := <-signalCh
			signame := s.String()
			var shuttingDown, gracefully bool
			switch s {
			case syscall.SIGHUP:
			case syscall.SIGINT:
				shuttingDown, gracefully = true, true
			case syscall.SIGTERM:
				shuttingDown, gracefully = true, true
			case syscall.SIGQUIT:
				shuttingDown, gracefully = true, false
			}

			if shuttingDown {
				var trailer bytes.Buffer
				trailer.WriteString(", shutting down ")
				if gracefully {
					trailer.WriteString("with grace")
				} else {
					trailer.WriteString("shamefully")
				}
				log.Printf("caught %s%s\n", signame, trailer.String())
				server.shutdown <- gracefully
			}
		}
	}()

}

func (server *Chatter) Close() {
	if server.listener != nil {
		start := time.Now()
		err := server.listener.Close()
		elapsed := time.Now().Sub(start)

		if err != nil {
			log.Printf("error occurred while closing socket: %s\n", err.Error())
		} else {
			log.Printf("closing listener (%v)\n", elapsed.String())
		}
	}
	if server.consul != nil {
		start := time.Now()
		err := server.consul.Agent().ServiceDeregister(server.id)
		elapsed := time.Now().Sub(start)
		if err != nil {
			log.Printf("error occurred while deregistering from consul: %s\n", err.Error())
		} else {
			log.Printf("deregistering from consul (%v)\n", elapsed.String())
		}
	}
	if server.plan != nil && server.plan.IsStopped() == false {
		start := time.Now()
		server.plan.Stop();
		elapsed := time.Now().Sub(start)
		log.Printf("stopping peer search (%v)\n", elapsed.String())
	}
}

func (server *Chatter) startSearchingForPeers() {
	wp := make(map[string]interface{})
	wp["type"] = "service"
	wp["service"] = "chatter"
	wp["handler_type"] = "http"

	wphhc := make(map[string]interface{})
	wphhc["path"] = "/catalog/service/chatter"

	wp["http_handler_config"] = wphhc
	plan, err := watch.Parse(wp)
	panicOnError(err)
	server.plan = plan
	log.Println("made a plan to find some peers")

	server.plan.Handler = func(idx uint64, data interface{}) {
		// TODO: should pay attention to idx

		var drops, adds int
		entries := data.([]*c.ServiceEntry)
		addressToServiceEntryMap := keyByHashableConnectionAddress(entries)

		server.connections.Each(func(addr gotomic.Hashable, conn gotomic.Thing) bool {
			_, exisitingConnectionStillRegistered := addressToServiceEntryMap[addr]

			if fmt.Sprint(addr) == server.conn.String() {
				server.indexedLog(idx, "skipping ourselves 1")
				return false
			}

			if server.listener != nil && fmt.Sprint(addr) == server.listener.Addr().String() {
				server.indexedLog(idx, "skipping ourselves 2")
				return false
			}

			if exisitingConnectionStillRegistered {
				log.Printf("already know about %s\n", addr)
				delete(addressToServiceEntryMap, addr)
			} else {
				switch connection := conn.(type) {
				case net.Conn:
					start := time.Now()
					connection.Close()
					elapsed := time.Now().Sub(start)
					server.indexedLog(idx, "%s [%v] closing\n", addr, elapsed.String())
				default:
					server.indexedLog(idx, "unexpected type in connection map: %v\n", reflect.TypeOf(conn))
				}
				drops++
			}

			return false // dont stop iterating!
		})

		// remaining connections are new guys, we don't use the value (ServiceEntry) yet
		for addr, _ := range addressToServiceEntryMap {


			if fmt.Sprint(addr) == server.conn.String() {
				server.indexedLog(idx, "skipping ourselves 1")
				continue
			}

			if server.listener != nil && fmt.Sprint(addr) == server.listener.Addr().String() {
				server.indexedLog(idx, "skipping ourselves 2")
				continue
			}

			server.indexedLog(idx, "kicking off a connection to %s\n", addr)

			go func(addressKey gotomic.Hashable) {


				server.indexedLog(idx, "asynchronously dialing %s\n", addressKey)

				start := time.Now()
				dialAddress := fmt.Sprint(addressKey)
				establishedConn, err := net.Dial("tcp", dialAddress)
				elapsed := time.Now().Sub(start)

				if err != nil {
					server.indexedLog(idx, "error dialing remote socket: %s\n", err.Error())
					return
				} else {
					server.indexedLog(idx, "%s dialing (%v)\n", addressKey, elapsed.String())
				}

				//greeting := make([]byte, 2)

				//start = time.Now()
				//establishedConn.Read(greeting)
				//elapsed = time.Now().Sub(start)
				//server.indexedLog(idx, "%s reading the greeting (%v)\n", addressKey, elapsed.String())

				start = time.Now()
				server.connections.Put(addressKey, establishedConn)
				elapsed = time.Now().Sub(start)
				server.indexedLog(idx, "%s mapping (%v)\n", addressKey, elapsed.String())
			}(addr)
			adds++
		}

		server.indexedLog(idx, "peer search handler fired with %d entries, %d adds, %d drops\n", len(entries), adds, drops)
	}

	go func() {
		err = plan.Run(consulConn.String())
		log.Printf("watch returned: %v\n", err)
		panicOnError(err)
	}()

	log.Println("started the search for peers")
}

func (server *Chatter) indexedLog(
	idx uint64,
	format string, v ...interface{}) {
	log.Print(
		fmt.Sprintf("%5d %s ", idx, server.conn.String()),
		fmt.Sprintf(format, v...))
}

func keyByHashableConnectionAddress(entries []*c.ServiceEntry) map[gotomic.Hashable]*c.ServiceEntry {
	rval := make(map[gotomic.Hashable]*c.ServiceEntry)
	for _, entry := range entries {
		remoteAddr := entry.Service.Address + ":" + strconv.Itoa(entry.Service.Port)
		rval[gotomic.StringKey(remoteAddr)] = entry
	}
	return rval
}

func panicOnError(e error) {
	if e != nil {
		panic(e)
	}
}

func (server *Chatter) awaitShutdown() {
	graceful := <-server.shutdown
	kindness := "violently"
	if graceful {
		kindness = "gracefully"
	}

	log.Printf("%s stopping chatter (%v)\n", kindness, server.conn.String())

	if graceful {
		server.Close()
	}
}

func buildConsulConfig(addr string) *c.Config {
	configConsul := c.DefaultConfig()

	if addr != "" {
		configConsul.Address = addr
	} else {
		configConsul.Address = "consul:8500"
	}

	return configConsul
}

////"method"
////"-"
////"timeout"
////"header"
////"tls_skip_verify"
//
//watchParams
////watchPlan, err := watch.Parse(watchParams)
////panicOnError(err)
////watchPlan.Run(configConsul.Address)
//
