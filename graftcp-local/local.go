package main

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jedisct1/dlog"
	"golang.org/x/net/proxy"
)

type modeT int

const (
	// AutoSelectMode select socks5 if socks5 is reachable, else HTTP proxy
	AutoSelectMode modeT = iota
	// RandomSelectMode select the reachable proxy randomly
	RandomSelectMode
	// OnlySocks5Mode force use socks5
	OnlySocks5Mode
	// OnlyHttpProxyMode force use HTTP proxy
	OnlyHttpProxyMode
	// DirectMode direct connect
	DirectMode
)

type Local struct {
	faddr *net.TCPAddr // Frontend address: graftcp-local address

	faddrString string

	socks5Dialer    proxy.Dialer
	httpProxyDialer proxy.Dialer
	directDialer    proxy.Dialer

	FifoFd *os.File

	selectMode modeT
}

func NewLocal(listenAddr, socks5Addr, socks5Username, socks5PassWord, httpProxyAddr string) *Local {
	listenTCPAddr, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		dlog.Fatalf("resolve frontend(%s) error: %s", listenAddr, err.Error())
	}
	local := &Local{
		faddr:       listenTCPAddr,
		faddrString: listenAddr,
	}
	local.directDialer = proxy.Direct

	socks5TCPAddr, err1 := net.ResolveTCPAddr("tcp", socks5Addr)
	httpProxyTCPAddr, err2 := net.ResolveTCPAddr("tcp", httpProxyAddr)
	if err1 != nil && err2 != nil {
		dlog.Fatalf(
			"neither %s nor %s can be resolved, resolve(%s): %v, resolve(%s): %v, please check the config for proxy",
			socks5Addr, httpProxyAddr, socks5Addr, err1, httpProxyAddr, err2)
	}
	if err1 == nil {
		var auth *proxy.Auth
		if socks5Username != "" {
			auth = &proxy.Auth{
				User:     socks5Username,
				Password: socks5PassWord,
			}
		}
		dialerSocks5, err := proxy.SOCKS5("tcp", socks5TCPAddr.String(), auth, proxy.Direct)
		if err != nil {
			dlog.Errorf("proxy.SOCKS5(%s) fail: %s", socks5TCPAddr.String(), err.Error())
		} else {
			local.socks5Dialer = dialerSocks5
		}
	}
	if err2 == nil {
		httpProxyURI, _ := url.Parse("http://" + httpProxyTCPAddr.String())
		dialerHttpProxy, err := proxy.FromURL(httpProxyURI, proxy.Direct)
		if err != nil {
			dlog.Errorf("proxy.FromURL(%v) err: %s", httpProxyURI, err.Error())
		} else {
			local.httpProxyDialer = dialerHttpProxy
		}
	}
	return local
}

// SetSelectMode set the select mode for l.
func (l *Local) SetSelectMode(mode string) {
	switch mode {
	case "auto":
		l.selectMode = AutoSelectMode
	case "random":
		l.selectMode = RandomSelectMode
	case "only_http_proxy":
		l.selectMode = OnlyHttpProxyMode
	case "only_socks5":
		l.selectMode = OnlySocks5Mode
	case "direct":
		l.selectMode = DirectMode
	}
}

func (l *Local) proxySelector() proxy.Dialer {
	if l == nil {
		return nil
	}
	switch l.selectMode {
	case AutoSelectMode:
		if l.socks5Dialer != nil {
			return l.socks5Dialer
		} else if l.httpProxyDialer != nil {
			return l.httpProxyDialer
		}
		return l.directDialer
	case RandomSelectMode:
		if l.socks5Dialer != nil && l.httpProxyDialer != nil {
			if rand.Intn(2) == 0 {
				return l.socks5Dialer
			}
			return l.httpProxyDialer
		} else if l.socks5Dialer != nil {
			return l.socks5Dialer
		}
		return l.httpProxyDialer
	case OnlySocks5Mode:
		return l.socks5Dialer
	case OnlyHttpProxyMode:
		return l.httpProxyDialer
	case DirectMode:
		return l.directDialer
	default:
		return l.socks5Dialer
	}
}

func (l *Local) Start() {
	ln, err := net.ListenTCP("tcp", l.faddr)
	if err != nil {
		dlog.Fatalf("net.ListenTCP(%s) err: %s", l.faddr.String(), err.Error())
	}
	defer ln.Close()
	dlog.Infof("graftcp-local start listening %s...", l.faddr.String())

	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			dlog.Errorf("accept err: %s", err.Error())
			continue
		}
		go l.HandleConn(conn)
	}
}

func getPidByAddr(localAddr, remoteAddr string, isTCP6 bool) (pid string, destAddr string) {
	inode, err := getInodeByAddrs(localAddr, remoteAddr, isTCP6)
	if err != nil {
		dlog.Errorf("getInodeByAddrs(%s, %s) err: %s", localAddr, remoteAddr, err.Error())
		return "", ""
	}
	for i := 0; i < 3; i++ { // try 3 times
		RangePidAddr(func(p, a string) bool {
			if hasIncludeInode(p, inode) {
				pid = p
				destAddr = a
				return false
			}
			return true
		})
		if pid != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid != "" {
		DeletePidAddr(pid)
	}
	return
}

func (l *Local) HandleConn(conn net.Conn) error {
	raddr := conn.RemoteAddr()
	var isTCP6 bool
	if strings.Contains(conn.LocalAddr().String(), "[") {
		isTCP6 = true
	}
	pid, destAddr := getPidByAddr(raddr.String(), conn.LocalAddr().String(), isTCP6)
	if pid == "" || destAddr == "" {
		dlog.Errorf("getPidByAddr(%s, %s) failed", raddr.String(), conn.LocalAddr().String())
		conn.Close()
		return fmt.Errorf("can't find the pid and destAddr for %s", raddr.String())
	}
	dlog.Infof("Request PID: %s, Source Addr: %s, Dest Addr: %s", pid, raddr.String(), destAddr)

	dialer := l.proxySelector()
	if dialer == nil {
		dlog.Errorf("bad dialer,  please check the config for proxy")
		conn.Close()
		return fmt.Errorf("bad dialer")
	}
	destConn, err := dialer.Dial("tcp", destAddr)
	if err != nil && l.selectMode == AutoSelectMode { // AutoSelectMode try direct
		dlog.Infof("dial %s direct", destAddr)
		destConn, err = net.Dial("tcp", destAddr)
	}
	if err != nil {
		dlog.Errorf("dialer.Dial(%s) err: %s", destAddr, err.Error())
		conn.Close()
		return err
	}
	readChan, writeChan := make(chan int64), make(chan int64)
	go pipe(conn, destConn, writeChan)
	go pipe(destConn, conn, readChan)
	<-writeChan
	<-readChan
	conn.Close()
	destConn.Close()
	return nil
}

func pipe(dst, src net.Conn, c chan int64) {
	n, _ := io.Copy(dst, src)
	now := time.Now()
	dst.SetDeadline(now)
	src.SetDeadline(now)
	c <- n
}

func (l *Local) UpdateProcessAddrInfo() {
	r := bufio.NewReader(l.FifoFd)
	for {
		line, _, err := r.ReadLine()
		if err != nil {
			dlog.Errorf("r.ReadLine err: %s", err.Error())
			break
		}
		copyLine := string(line)
		// dest_ipaddr:dest_port:pid
		s := strings.Split(copyLine, ":")
		if len(s) < 3 {
			dlog.Errorf("r.ReadLine(): %s", copyLine)
			continue
		}
		var (
			pid  string
			addr string
		)
		if len(s) > 3 { // IPv6
			pid = s[len(s)-1]
			destPort := s[len(s)-2]
			destIP := copyLine[:len(copyLine)-2-len(pid)-len(destPort)]
			addr = "[" + destIP + "]:" + destPort
		} else { // IPv4
			pid = s[2]
			addr = s[0] + ":" + s[1]
		}
		go StorePidAddr(pid, addr)
	}
}

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
}
