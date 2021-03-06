// strongly inspired by http://golang.org/src/net/smtp/smtp.go

package core

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/textproto"
	"strings"
	"time"
)

// smtpClient represent an SMTP client
type smtpClient struct {
	text    *textproto.Conn
	route   *Route
	conn    net.Conn
	connTLS *tls.Conn
	// map of supported extensions
	ext map[string]string
	// whether the Client is using TLS
	tls bool
	// supported auth mechanisms
	auth []string
}

// newSMTPClient return a connected SMTP client
func newSMTPClient(routes *[]Route) (client *smtpClient, err error) {
	for _, route := range *routes {
		localIPs := []net.IP{}
		remoteAddresses := []net.TCPAddr{}
		// no mix beetween failover and round robin for local IP
		failover := strings.Count(route.LocalIp.String, "&") != 0
		roundRobin := strings.Count(route.LocalIp.String, "|") != 0
		if failover && roundRobin {
			return nil, fmt.Errorf("failover and round-robin are mixed in route %d for local IP", route.Id)
		}

		// Contient les IP sous forme de string
		var sIps []string

		// On a une seule IP locale
		if !failover && !roundRobin {
			sIps = []string{route.LocalIp.String}
		} else { // multiple locales ips
			var sep string
			if failover {
				sep = "&"
			} else {
				sep = "|"
			}
			sIps = strings.Split(route.LocalIp.String, sep)

			// if roundRobin we need to shuffle IPs
			rSIps := make([]string, len(sIps))
			perm := rand.Perm(len(sIps))
			for i, v := range perm {
				rSIps[v] = sIps[i]
			}
			sIps = rSIps
			rSIps = nil
		}

		// IP string to net.IP
		for _, ipStr := range sIps {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				return nil, errors.New("invalid IP " + ipStr + " found in localIp routes: " + route.LocalIp.String)
			}
			localIPs = append(localIPs, ip)
		}

		// remoteAdresses
		// Hostname or IP
		// IP ?
		ip := net.ParseIP(route.RemoteHost)
		if ip != nil { // ip
			remoteAddresses = append(remoteAddresses, net.TCPAddr{
				IP:   ip,
				Port: int(route.RemotePort.Int64),
			})
			// hostname
		} else {
			ips, err := net.LookupIP(route.RemoteHost)
			// TODO: no such host -> perm failure
			if err != nil {
				return nil, err
			}
			for _, i := range ips {
				remoteAddresses = append(remoteAddresses, net.TCPAddr{
					IP:   i,
					Port: int(route.RemotePort.Int64),
				})
			}
		}

		// try routes & returns first OK
		for _, localIP := range localIPs {
			for _, remoteAddr := range remoteAddresses {
				// IPv4 <-> IPv4 or IPv6 <-> IPv6
				if IsIPV4(localIP.String()) != IsIPV4(remoteAddr.IP.String()) {
					continue
				}
				// TODO timeout en config
				//err, conn := dial(remoteAddr, localIP.String())

				localAddr, err := net.ResolveTCPAddr("tcp", localIP.String()+":0")
				if err != nil {
					return nil, errors.New("bad local IP: " + localIP.String() + ". " + err.Error())
				}

				// Dial timeout
				connectTimer := time.NewTimer(time.Duration(30) * time.Second)
				done := make(chan error, 1)
				var conn net.Conn
				go func() {
					conn, err = net.DialTCP("tcp", localAddr, &remoteAddr)
					done <- err
				}()

				select {
				case err = <-done:
					if err == nil {
						client := &smtpClient{
							conn: conn,
						}
						client.text = textproto.NewConn(conn)
						_, _, err := client.text.ReadCodeLine(220)
						if err == nil {
							client.route = &route
							return client, nil
						}
					}
					return nil, err
				// Timeout
				case <-connectTimer.C:
					err = errors.New("timeout")
				}
				Log.Debug("unable to get a SMTP client", localIP, "->", remoteAddr.IP.String(), ":", remoteAddr.Port, "-", err.Error())
			}
		}
	}
	// All routes have been tested -> Fail !
	return nil, errors.New("unable to get a client, all routes have been tested")
}

// CloseConn close connection
func (s *smtpClient) close() error {
	return s.text.Close()
}

// cmd send a command and return reply
func (s *smtpClient) cmd(timeoutSeconds, expectedCode int, format string, args ...interface{}) (int, string, error) {
	var id uint
	var err error
	timeout := make(chan bool, 1)
	done := make(chan bool, 1)
	timer := time.AfterFunc(time.Duration(timeoutSeconds)*time.Second, func() {
		timeout <- true
	})
	defer timer.Stop()
	go func() {
		id, err = s.text.Cmd(format, args...)
		done <- true
	}()

	select {
	case <-timeout:
		return 0, "", errors.New("server do not reply in time -> timeout")
	case <-done:
		if err != nil {
			return 0, "", err
		}
		s.text.StartResponse(id)
		defer s.text.EndResponse(id)
		code, msg, err := s.text.ReadResponse(expectedCode)
		return code, msg, err
	}
}

// Extension reports whether an extension is support by the server.
func (s *smtpClient) Extension(ext string) (bool, string) {
	if s.ext == nil {
		return false, ""
	}
	ext = strings.ToUpper(ext)
	param, ok := s.ext[ext]
	return ok, param
}

// TLSGetVersion  returne TLS/SSL version
func (s *smtpClient) TLSGetVersion() string {
	if !s.tls {
		return "no TLS"
	}
	return tlsGetVersion(s.connTLS.ConnectionState().Version)
}

// TLSGetCipherSuite return cipher suite use for TLS connection
func (s *smtpClient) TLSGetCipherSuite() string {
	if !s.tls {
		return "No TLS"
	}
	return tlsGetCipherSuite(s.connTLS.ConnectionState().CipherSuite)
}

// RemoteAddr return remote address (IP:PORT)
func (s *smtpClient) RemoteAddr() string {
	if s.tls {
		return s.connTLS.RemoteAddr().String()
	}
	return s.conn.RemoteAddr().String()
}

// LocalAddr return local address (IP:PORT)
func (s *smtpClient) LocalAddr() string {
	if s.tls {
		return s.connTLS.LocalAddr().String()
	}
	return s.conn.LocalAddr().String()
}

// SMTP commands

// SMTP NOOP
func (s *smtpClient) Noop() (code int, msg string, err error) {
	return s.cmd(30, 200, "NOOP")
}

// Hello: try EHLO, if failed HELO
func (s *smtpClient) Hello() (code int, msg string, err error) {
	code, msg, err = s.Ehlo()
	if err == nil {
		return
	}
	return s.Helo()
}

// SMTP HELO
func (s *smtpClient) Ehlo() (code int, msg string, err error) {
	code, msg, err = s.cmd(10, 250, "EHLO %s", Cfg.GetMe())
	if err != nil {
		return code, msg, err
	}
	ext := make(map[string]string)
	extList := strings.Split(msg, "\n")
	if len(extList) > 1 {
		extList = extList[1:]
		for _, line := range extList {
			args := strings.SplitN(line, " ", 2)
			if len(args) > 1 {
				ext[args[0]] = args[1]
			} else {
				ext[args[0]] = ""
			}
		}
	}
	if mechs, ok := ext["AUTH"]; ok {
		s.auth = strings.Split(mechs, " ")
	}
	s.ext = ext
	return
}

// SMTP HELO
func (s *smtpClient) Helo() (code int, msg string, err error) {
	s.ext = nil
	code, msg, err = s.cmd(30, 250, "HELO %s", Cfg.GetMe())
	return
}

// StartTLS sends the STARTTLS command and encrypts all further communication.
func (s *smtpClient) StartTLS(config *tls.Config) (code int, msg string, err error) {
	s.tls = false
	code, msg, err = s.cmd(30, 220, "STARTTLS")
	if err != nil {
		return
	}
	s.connTLS = tls.Client(s.conn, config)
	s.text = textproto.NewConn(s.connTLS)
	code, msg, err = s.Ehlo()
	if err != nil {
		return
	}
	s.tls = true
	return
}

// AUTH
func (s *smtpClient) Auth(a DeliverdAuth) (code int, msg string, err error) {
	encoding := base64.StdEncoding
	mech, resp, err := a.Start(&ServerInfo{Cfg.GetMe(), s.tls, s.auth})
	if err != nil {
		s.Quit()
		return
	}
	resp64 := make([]byte, encoding.EncodedLen(len(resp)))
	encoding.Encode(resp64, resp)
	code, msg64, err := s.cmd(30, 0, "AUTH %s %s", mech, resp64)
	for err == nil {
		var msg []byte
		switch code {
		case 334:
			msg, err = encoding.DecodeString(msg64)
		case 235:
			// the last message isn't base64 because it isn't a challenge
			msg = []byte(msg64)
		default:
			err = &textproto.Error{Code: code, Msg: msg64}
		}
		if err == nil {
			resp, err = a.Next(msg, code == 334)
		}
		if err != nil {
			// abort the AUTH
			s.cmd(10, 501, "*")
			s.Quit()
			break
		}
		if resp == nil {
			break
		}
		resp64 = make([]byte, encoding.EncodedLen(len(resp)))
		encoding.Encode(resp64, resp)
		code, msg64, err = s.cmd(30, 0, string(resp64))
	}
	return
}

// MAIL
func (s *smtpClient) Mail(from string) (code int, msg string, err error) {
	return s.cmd(30, 250, "MAIL FROM:<%s>", from)
}

// RCPT
func (s *smtpClient) Rcpt(to string) (code int, msg string, err error) {
	code, msg, err = s.cmd(30, -1, "RCPT TO:<%s>", to)
	if code != 250 && code != 251 {
		err = errors.New(msg)
	}
	return
}

// DATA
type dataCloser struct {
	s *smtpClient
	io.WriteCloser
}

// Data issues a DATA command to the server and returns a writer that
// can be used to write the data. The caller should close the writer
// before calling any more methods on c.
func (s *smtpClient) Data() (*dataCloser, int, string, error) {
	code, msg, err := s.cmd(30, 354, "DATA")
	if err != nil {
		return nil, code, msg, err
	}
	return &dataCloser{s, s.text.DotWriter()}, code, msg, nil
}

// QUIT
func (s *smtpClient) Quit() (code int, msg string, err error) {
	code, msg, err = s.cmd(10, 221, "QUIT")
	s.text.Close()
	return
}
