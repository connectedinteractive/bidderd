package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"

	"github.com/valyala/fasthttp"
)

const (
	ACSIp       = "127.0.0.1"
	ACSPort     = 9986
	BankerIp    = "127.0.0.1"
	BankerPort  = 9985
	BidderWin   = 7653
	BidderEvent = 7652
	BidderError = 7651
)

var bidderPort int
var wg sync.WaitGroup

// http client to pace agents (note that it's pointer)
var client = &http.Client{}

func printPortConfigs() {
	log.Printf("Bidder port: %d", bidderPort)
	log.Printf("Win port: %d", BidderWin)
	log.Printf("Event port: %d", BidderEvent)
}

func setupHandlers(agents []Agent) {
	m := func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/auctions":
			fastHandleAuctions(ctx, agents)
		default:
			ctx.Error("not found", fasthttp.StatusNotFound)
		}
	}

	go fasthttp.ListenAndServe(fmt.Sprintf(":%d", bidderPort), m)
	log.Println("Started Bid Mux")
}

func cleanup(agents []Agent) {
	stopRedisSubscriber()
	// Implement remove agent from ACS
	for _, agent := range agents {
		agent.UnregisterAgent(client, ACSIp, ACSPort)
	}

	fmt.Println("Leaving...")

	for {
		wg.Done()
	}
}

func main() {
	var agentsConfigFile = flag.String("config", "agents.json", "Configuration file in JSON.")
	flag.IntVar(&bidderPort, "port", 7654, "Port to listen on for router")

	flag.Parse()
	if *agentsConfigFile == "" {
		log.Fatal("You should provide a configuration file.")
	}

	setupClient()
	go startRedisSubscriber()
	wg.Add(1)

	printPortConfigs()

	// load configuration
	agents, err := LoadAgentsFromFile(*agentsConfigFile)

	if err != nil {
		log.Fatal(err)
	}
	for _, agent := range agents {
		agent.RegisterAgent(client, ACSIp, ACSPort)
		agent.StartPacer(client, BankerIp, BankerPort)
	}

	StartStatOutput()
	setupHandlers(agents)

	go fasthttp.ListenAndServe(fmt.Sprintf(":%d", BidderEvent), eventMux)
	log.Println("Started event Mux")

	go fasthttp.ListenAndServe(fmt.Sprintf(":%d", BidderError), errorMux)
	log.Println("Started error Mux")

	go fasthttp.ListenAndServe(fmt.Sprintf(":%d", BidderWin), winMux)
	log.Println("Started Win Mux")

	wg.Add(3)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	go func() {
		<-c
		cleanup(agents)
		os.Exit(1)
	}()

	wg.Wait()
}
