package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"strconv"
	"strings"
	"time"

	_ "github.com/breml/rootcerts"
	"github.com/mmcdole/gofeed"
	"golang.org/x/time/rate"
	"gopkg.in/irc.v3"
)

var messageBeat chan bool
var firstMessage chan bool
var client *irc.Client
var messageLimit = rate.NewLimiter(1, 3)

var (
	feedURL = flag.String("feed", "", "the url to the feed")
	channel = flag.String("channel", "", "channel")
)

func main() {
	nick := flag.String("nick", "", "the ircnick you want")
	from := flag.String("ip", "[::1]", "src address")
	server := flag.String("ircserver", "", "server")
	nsuser := flag.String("nickservuser", "", "NICKSERV username")
	nspass := flag.String("nickservpasswd", "", "NICKSERV password")
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

			if m.Command == "366" {
				log.Printf("started polling RSS")
				go lookForUpdates()
			}

			if m.Command == "001" {
				// 001 is a welcome event, so we join channels there
				if *nsuser != "" {
					c.Write(fmt.Sprintf("PRIVMSG NickServ :IDENTIFY %s %s", *nsuser, *nspass))
				} else {
					c.Write(fmt.Sprintf("JOIN %s", *channel))
				}
			} else if m.Command == "PRIVMSG" && c.FromChannel(m) {
				// Create a handler on all messages.
			} else if m.Command == "NOTICE" {
				if *nsuser != "" {
					if strings.ToLower(m.Params[0]) == *nick {
						c.Write(fmt.Sprintf("JOIN %s", *channel))
					}
				}
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

func loadLastKnownPost() int64 {
	b, err := ioutil.ReadFile("./last-known-post")
	if err != nil {
		return 0
	}

	i, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0
	}
	return i
}

func saveNewLastKnownPost(in int64) {
	ioutil.WriteFile("./last-known-post", []byte(fmt.Sprintf("%d", in)), 0660)
}

func lookForUpdates() {
	lastKnownPost := loadLastKnownPost()
	n := 1
	for {
		time.Sleep(time.Second * time.Duration(n))
		n++
		if n == 60 {
			n = 59
		}
		fp := gofeed.NewParser()
		fp.UserAgent = "github.com/benjojo/dumb-rss-to-irc"
		feed, err := fp.ParseURL(*feedURL)
		if err != nil {
			log.Printf("failed to handle RSS error %v", err)
			// handle error
			continue
		}

		for i := len(feed.Items) - 1; i != -1; i-- {
			v := feed.Items[i]
			if v.PublishedParsed.Unix() > lastKnownPost {
				client.WriteMessage(&irc.Message{
					Command: "PRIVMSG",
					Params:  []string{*channel, figureOutTitle(v)},
				})
				n = 1
				saveNewLastKnownPost(v.PublishedParsed.Unix())
				lastKnownPost = v.PublishedParsed.Unix()
				time.Sleep(time.Second)
				log.Printf("posted %v", figureOutTitle(v))
			}
		}
		log.Printf("Fetched RSS feed, got %d items", len(feed.Items))
	}
}

func figureOutTitle(in *gofeed.Item) string {
	if len(in.Description) < 500 {
		return strings.Replace(strings.Replace(in.Description, "\n", "", 0), "\r", "", 0)
	}

	return strings.Replace(strings.Replace(in.Title, "\n", "", 0), "\r", "", 0)
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
