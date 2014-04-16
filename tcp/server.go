package tcp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"

	"github.com/huin/mqtt"
	"github.com/ninjablocks/mqtt-proxy/conf"
	"github.com/ninjablocks/mqtt-proxy/proxy"
	"github.com/ninjablocks/mqtt-proxy/store"
	"github.com/ninjablocks/mqtt-proxy/util"
)

type TcpServer struct {
	proxy *proxy.MQTTProxy
	store store.Store
}

func CreateTcpServer(proxy *proxy.MQTTProxy) *TcpServer {

	store := store.NewMysqlStore(&proxy.Conf.BlockStoreMysql)

	return &TcpServer{
		proxy: proxy,
		store: store,
	}
}

func (t *TcpServer) StartServer(conf *conf.MqttConfiguration) {

	log.Printf("[tcp] listening on %s", conf.ListenAddress)

	listener, err := t.startListener(conf)

	if err != nil {
		log.Fatalln("error listening:", err.Error())
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Client error: %s", err)
		} else {
			go t.clientHandler(conn)
		}
	}

}

func (t *TcpServer) startListener(conf *conf.MqttConfiguration) (net.Listener, error) {
	if conf.Cert != "" {
		cert, err := tls.LoadX509KeyPair(conf.Cert, conf.Key)

		if err != nil {
			log.Fatalf("server: loadkeys: %s", err)
		}

		config := tls.Config{Certificates: []tls.Certificate{cert}}

		return tls.Listen("tcp", conf.ListenAddress, &config)
	} else {
		return net.Listen("tcp", conf.ListenAddress)
	}

}

func (t *TcpServer) clientHandler(conn net.Conn) {

	defer conn.Close()

	// create channels for the return messages from the client
	cmr := util.CreateMqttTcpMessageReader(conn)

	go cmr.ReadMqttMessages()

	// This needs to be distributed across all servers
	backend := t.proxy.Conf.BackendServers[0]

	p, err := CreateTcpProxyConn(conn, backend)

	if err != nil {
		log.Printf("[serv] Error creating proxy connection - %s", err)
		sendServerUnavailable(conn)
		return
	}

	// do the authentication up front before going into normal operation
	if err = t.handleAuth(cmr, p); err != nil {
		log.Printf("[serv] Error authenticating connection - %s", err)
		sendBadUsernameOrPassword(p.cConn)
		return
	}

	// create channels for the return messages from the backend
	pmr := util.CreateMqttTcpMessageReader(p.pConn)

	go pmr.ReadMqttMessages()

Loop:
	for {

		select {

		case msg := <-cmr.InMsgs:

			util.DebugMQTT("client", conn, msg)
			msg = p.rewriter.RewriteIngress(msg)

			// write to the proxy connection
			if err := msg.Encode(p.pConn); err != nil {
				log.Println("[serv] proxy connection error - %s", err)
				break Loop
			}

		case err := <-cmr.InErrors:
			log.Printf("[serv] client connection read error - %s", err)
			break Loop

		case msg := <-pmr.InMsgs:

			util.DebugMQTT("proxy", conn, msg)
			msg = p.rewriter.RewriteEgress(msg)

			// write to the client connection
			if err := msg.Encode(p.cConn); err != nil {
				log.Println("[serv] proxy connection error - %s", err)
				break Loop
			}

		case err := <-pmr.InErrors:
			log.Printf("[serv] proxy connection read error - %s", err)
			break Loop
		}

	}

	// TODO Output stats from the proxy connection
	log.Println("[serv] Finished")

}

func (t *TcpServer) handleAuth(cmr *util.MqttTcpMessageReader, proxyConn *TcpProxyConn) error {

	select {
	case msg := <-cmr.InMsgs:

		util.DebugMQTT("auth", proxyConn.cConn, msg)

		switch cmsg := msg.(type) {
		case *mqtt.Connect:

			authUser, err := t.store.FindUser(cmsg.Username)

			if err != nil {
				return err
			}

			proxyConn.rewriter = t.proxy.MqttMsgRewriter(authUser)

			msg = proxyConn.rewriter.RewriteIngress(msg)

			if err := msg.Encode(proxyConn.pConn); err != nil {
				log.Println("[serv] proxy connection error - %s", err)
				return err
			}

			return nil

		}
		// anything else is bad
		return errors.New(fmt.Sprintf("expected connect got - %v", reflect.TypeOf(msg)))

	case err := <-cmr.InErrors:
		return err
	}

}

func sendBadUsernameOrPassword(conn net.Conn) {
	log.Printf("[serv] bad username / password %s %s", conn.LocalAddr(), conn.RemoteAddr())
	connAck := &mqtt.ConnAck{
		ReturnCode: mqtt.RetCodeBadUsernameOrPassword,
	}
	connAck.Encode(conn)
}

func sendServerUnavailable(conn net.Conn) {
	log.Printf("[serv] server unavailable %s %s", conn.LocalAddr(), conn.RemoteAddr())
	connAck := &mqtt.ConnAck{
		ReturnCode: mqtt.RetCodeServerUnavailable,
	}
	connAck.Encode(conn)
}
