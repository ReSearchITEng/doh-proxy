package dohProxy

import (
	"fmt"
	"github.com/miekg/dns"
	"net"
	"net/http"
	"strconv"
)

type Stub struct {
	ListenAddr       string
	UpstreamAddr     string
	UpstreamProtocol string // tcp or udp
	UseCache         bool
}

var (
	client *dns.Client
	conn   *dns.Conn
	cache  *Cache
)

func (stub Stub) ensureConn() error {
	if client == nil {
		client = &dns.Client{
			Net: stub.UpstreamProtocol,
			ReadTimeout: 5,
			WriteTimeout: 5,
			DialTimeout: 5,
		}
	}
	if conn != nil {
		return nil
	}
	conn_, err := client.Dial(stub.UpstreamAddr)
	if err != nil {
		Log.Errorf("connect to upstream server %v://%v failed: %v",
			stub.UpstreamProtocol, stub.UpstreamAddr, err)
		return err
	}
	conn = conn_
	return nil
}

func (stub Stub) answer(w http.ResponseWriter, r *http.Request) {
	accept_in_req := r.Header.Get("Accept")
	if accept_in_req != "" && accept_in_req != "*/*" && accept_in_req != ContentType {
		Log.Errorf("request content type not supported: %v", accept_in_req)
		w.Header().Add("content-type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, err := w.Write([]byte("request content type not supported."))
		if err != nil {
			Log.Errorf("write message failed: %v", err)
			return
		}
		return
	}
	q, err := stub.generateMsgFromReq(r)
	if err != nil {
		Log.Errorf("get message from request failed: %v", err)
		w.Header().Add("content-type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte("get message from request failed."))
		if err != nil {
			Log.Errorf("write response failed: %v", err)
			return
		}
		return
	}

	if stub.UseCache {
		rMsg := cache.Get(q)
		if rMsg != nil {
			rMsg.Id = q.Id
			Log.Infof("resolved from cache")
			stub.writeAnswer(rMsg, w)
			return
		}
	}

	rMsg, err := stub.relay(q)
	if err != nil {
		Log.Errorf("error when querying upstream: %v", err)
		w.Header().Add("content-type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte("error when querying upstream."))
		if err != nil {
			Log.Errorf("write response failed: %v", err)
			return
		}
		return
	}
	stub.writeAnswer(rMsg, w)
}

func (stub Stub) writeAnswer(rMsg *dns.Msg, w http.ResponseWriter) {
	bytes_4_write, err := rMsg.Pack()
	if err != nil {
		Log.Errorf("error when querying upstream: %v", err)
		w.Header().Add("content-type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte("error when querying upstream."))
		if err != nil {
			Log.Errorf("write response failed: %v", err)
			return
		}
		return
	}
	w.WriteHeader(200)
	_, err = w.Write(bytes_4_write)
	if err != nil {
		Log.Errorf("error when writing response: %v", err)
		return
	}
}

func (stub Stub) relay(msg *dns.Msg) (*dns.Msg, error) {
	err := stub.ensureConn()
	if err != nil {
		client = nil
		conn = nil
		return nil, fmt.Errorf("client connecting error")
	}
	rMsg, _, err := client.ExchangeWithConn(msg, conn)
	if err != nil {
		client = nil
		conn = nil
		Log.Errorf("error when relaying query: %v", err)
		return nil, err
	}
	if stub.UseCache {
		msgch := make(chan *dns.Msg)
		defer close(msgch)
		go cache.Insert(msgch)
		msgch <- rMsg
	}
	Log.Debugf("upstream answer: %v", rMsg)
	Log.Infof("resolved from upstream for: %v", rMsg.Question[0].String())
	return rMsg, nil
}

func (stub Stub) generateMsgFromReq(r *http.Request) (*dns.Msg, error) {
	qMsg := new(dns.Msg)
	qMsg.Id = dns.Id()
	qMsg.Response = false
	qMsg.Opcode = dns.OpcodeQuery
	qMsg.Authoritative = false
	qMsg.Truncated = false
	qMsg.RecursionAvailable = false
	qMsg.RecursionDesired = true
	qMsg.AuthenticatedData = true
	qMsg.CheckingDisabled = false

	qURL := r.URL.Query()
	qCT := qURL.Get("ct")
	if qCT != "" && qCT != ContentType {
		Log.Errorf("content type not supported: %v", qCT)
		return nil, fmt.Errorf("content type not supported")
	}

	qName := qURL.Get("name")
	qName = dns.CanonicalName(qName)
	if qName == "" {
		Log.Errorf("question name invalid: %v", qName)
		return nil, fmt.Errorf("question name invalid: %v", qName)
	}

	qType := qURL.Get("type")
	itype, err := strconv.Atoi(qType)
	if err != nil {
		Log.Errorf("question type invalid: %v", itype)
		return nil, fmt.Errorf("question type invalid")
	}
	qMsg = qMsg.SetQuestion(qName, uint16(itype))

	qSubnet := qURL.Get("edns_client_subnet")
	ip, ipnet, err := net.ParseCIDR(qSubnet)
	if ip == nil {
		ip = net.ParseIP(qSubnet)
	}
	if err != nil || ip == nil {
		Log.Errorf("question subnet invalid: %v", itype)
		return nil, fmt.Errorf("question type invalid")
	}
	subnet := new(dns.EDNS0_SUBNET)
	subnet.Family = 0
	subnet.Code = dns.EDNS0SUBNET
	subnet.SourceScope = 0
	subnet.Address = ip
	is_ip4 := ip.To4() != nil
	ones_ := 32
	if is_ip4 {
		subnet.Family = 1
	} else {
		subnet.Family = 2
	}
	if ipnet != nil {
		ones_, _ = ipnet.Mask.Size()
		subnet.SourceNetmask = uint8(ones_)
	} else {
		if is_ip4 {
			subnet.SourceNetmask = 32
		} else {
			subnet.SourceNetmask = 128
		}
	}

	Log.Infof("will query name: %v, type: %v, client_subnet: %v", qName, qType, qSubnet)

	ReplaceEDNS0Subnet(qMsg, subnet)
	return qMsg, nil
}

func (stub Stub) Run() {
	if stub.UseCache {
		cache = NewCache()
	}
	http.HandleFunc("/resolve", stub.answer)
	Log.Infof("running stub server http://%v <--> %v://%v ...",
		stub.ListenAddr, stub.UpstreamProtocol, stub.UpstreamAddr)
	err := http.ListenAndServe(stub.ListenAddr, nil)
	if err != nil {
		Log.Fatalf("stub server running into error: %v", err)
	}
	_ = conn.Close()
	Log.Info("stopping stub server...")
}
