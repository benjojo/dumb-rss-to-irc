package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/breml/rootcerts"
	"golang.org/x/time/rate"
	"gopkg.in/irc.v3"
)

var messageBeat chan bool
var firstMessage chan bool
var client *irc.Client
var safeLock sync.Mutex
var messageLimit = rate.NewLimiter(1, 3)

var (
	doneURLs = make(map[string]time.Time)
)

func main() {
	nick := flag.String("nick", "", "the ircnick you want")
	from := flag.String("ip", "[::1]", "src address")
	server := flag.String("ircserver", "", "server")
	channel := flag.String("channel", "", "channel")
	flag.Parse()

	if *nick == "" || *server == "" || *channel == "" {
		log.Fatalf("You need to set -nick -ircserver and -channel")
	}

	syslogger, err := syslog.New(syslog.LOG_INFO, "irc")
	if err != nil {
		log.Fatalln(err)
	}

	log.SetOutput(syslogger)

	messageLimit = rate.NewLimiter(rate.Limit(0.5), 1)

	localAddrDialier := &net.Dialer{
		LocalAddr: &net.TCPAddr{
			IP:   net.ParseIP(*from),
			Port: 0,
		},
	}

	conn, err := tls.DialWithDialer(localAddrDialier, "tcp", *server, &tls.Config{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Fatalln(err)
	}

	messageBeat = make(chan bool)
	firstMessage = make(chan bool, 10)
	go ircKeepalive()

	if *nick == "NONE" {
		log.Fatalf("You must set a nick")
	}

	seenMsgBefore := false
	config := irc.ClientConfig{
		Nick: *nick,
		User: *nick,
		Name: *nick,
		Handler: irc.HandlerFunc(func(c *irc.Client, m *irc.Message) {
			b, _ := json.Marshal(m)
			log.Printf("%#v", string(b))

			messageBeat <- true

			if !seenMsgBefore {
				firstMessage <- true
				seenMsgBefore = true
			}

			if m.Command == "001" {
				// 001 is a welcome event, so we join channels there
				// c.Write("PRIVMSG NickServ :IDENTIFY aaaa bbbb")
				c.Write(fmt.Sprintf("JOIN %s", *channel))
			} else if m.Command == "PRIVMSG" && c.FromChannel(m) {
				// Create a handler on all messages.
				w := strings.Split(m.Param(1), " ")

				for _, v := range w {
					_, err := url.Parse(v)
					if err == nil {
						// we have a URL!
						urlTitle := getURLTitle(v)
						if urlTitle != "" {
							if messageLimit.Allow() {
								client.WriteMessage(&irc.Message{
									Command: "PRIVMSG",
									Params: []string{
										*channel,
										urlTitle,
									},
								})
							}
						}
					}
				}
			} else if m.Command == "NOTICE" {
				// if strings.ToLower(m.Params[0]) == "chill-urls" {
				// c.Write("JOIN ##asdasd")
				// }
			} else if m.Command == "MODE" {
				// Auto deop
				if m.Param(0) == *channel && m.Param(1) == "+o" && m.Param(2) == *nick {
					c.Write(fmt.Sprintf("MODE %s -o %s", *channel, *nick))
				}
			}
		}),
	}

	// Create the client
	client = irc.NewClient(conn, config)
	err = client.Run()
	if err != nil {
		log.Fatalln(err)
	}
}

func getURLTitle(in string) string {
	if !doneURLs[in].IsZero() {
		if time.Since(doneURLs[in]) < time.Hour {
			return ""
		}
	}
	doneURLs[in] = time.Now()

	hc := http.Client{}
	headreq, err := hc.Head(in)
	if err != nil {
		log.Printf("failed to get link %v", err)
		return ""
	}
	if strings.Contains(headreq.Header.Get("content-type"), "html") {
		return getHTMLTitle(in)
	}

	return fmt.Sprintf("%s: %s [%d kb]", headreq.Request.URL.Host, headreq.Header.Get("content-type"), headreq.ContentLength/1024)
}

func getHTMLTitle(in string) string {
	rr, err := http.NewRequest("GET", in, nil)

	if strings.Contains(in, "youtube.com") {
		// rr.Header.Set("User-Agent", "curl/7.68.0")
		rr.Header.Set("User-Agent", "please just show the title")
		rr.Header.Set("Accept", "*/*")
	}

	if strings.Contains(in, "twitter.com") {
		in = strings.Replace(in, "twitter.com/", "nitter.eu/", 1)
		rr, err = http.NewRequest("GET", in, nil)

	}

	hc := http.Client{}
	req, err := hc.Do(rr)
	if err != nil {
		log.Printf("failed to get link %v", err)
		return ""
	}

	jttr := io.LimitReader(req.Body, 256000)
	justTheTip, err := ioutil.ReadAll(jttr)

	re := regexp.MustCompile(`<title.*?>(.*)</title>`) //
	if strings.Contains(in, "youtube.com") {
		re = regexp.MustCompile(`,"title":{"simpleText":"(.*)"},"description":{"s`) // <title.*?>(.*)</title>
	}

	// if strings.Contains(string(justTheTip), "mosquitoes") {
	// 	// panic("yo")
	// }
	// fmt.Printf(string(justTheTip))
	submatchall := re.FindAllStringSubmatch(string(justTheTip), -1)
	for _, element := range submatchall {
		return strings.Trim(fmt.Sprintf("%s: %s", req.Request.URL.Host, element[1]), "\r\n \t")
	}

	log.Printf("failed to find a html tag: ")
	return ""
}

func ircKeepalive() {
	tt := time.NewTimer(time.Second)
	lastPing := time.Now()
	for {
		select {
		case <-tt.C:
			if time.Since(lastPing) > time.Minute*5 {
				log.Fatalf("It's been too long since the last IRC message, blowing up")
			}
			break
		case <-messageBeat:
			lastPing = time.Now()
		}
	}
}
