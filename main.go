package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/valyala/fasthttp"

	openrtb "gopkg.in/bsm/openrtb.v2"
)

const (
	ACSIp       string = "127.0.0.1"
	ACSPort            = 9986
	BankerIp           = "127.0.0.1"
	BankerPort         = 9985
	BidderPort         = 7654
	BidderWin          = 7653
	BidderEvent        = 7652
)

func timeTrack(start time.Time, name string) {
	// elapsed := time.Since(start)
	// log.Printf("%s request took %s", name, elapsed)
}

func track(fn http.HandlerFunc, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		defer timeTrack(time.Now(), name)
		fn(w, req)
	}
}

func printPortConfigs() {
	log.Printf("Bidder port: %d", BidderPort)
	log.Printf("Win port: %d", BidderWin)
	log.Printf("Event port: %d", BidderEvent)
}

func fastHandleAuctions(ctx *fasthttp.RequestCtx, agents []Agent) {
	var (
		ok    bool = true
		tmpOk bool = true
	)

	// enc := json.NewEncoder(w)
	// body, _ := ioutil.ReadAll(r.Body)
	// fmt.Println(string(body))
	var req *openrtb.BidRequest
	err := json.Unmarshal(ctx.PostBody(), &req)
	// req, err := openrtb.ParseRequest(r.Body)

	if err != nil {
		log.Println("ERROR", err.Error())
		ctx.SetStatusCode(204)
		return
	}

	// log.Println("INFO Received bid request", req.ID)

	ids := externalIdsFromRequest(req)
	res := emptyResponseWithOneSeat(req)

	for _, agent := range agents {
		res, tmpOk = agent.DoBid(req, res, ids)
		ok = tmpOk || ok
	}

	BidIncoming()

	if ok {
		ctx.Response.Header.Set("Content-type", "application/json")
		ctx.Response.Header.Set("x-openrtb-version", "2.1")
		ctx.SetStatusCode(http.StatusOK)

		bytes, _ := json.Marshal(res)
		ctx.SetBody(bytes)

		return
	}
	log.Println("No bid.")
	ctx.SetStatusCode(204)
}

func main() {
	var agentsConfigFile = flag.String("config", "agents.json", "Configuration file in JSON.")
	flag.Parse()
	if *agentsConfigFile == "" {
		log.Fatal("You should provide a configuration file.")
	}

	printPortConfigs()

	// http client to pace agents (note that it's pointer)
	client := &http.Client{}

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

	m := func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/auctions":
			fastHandleAuctions(ctx, agents)
		default:
			ctx.Error("not found", fasthttp.StatusNotFound)
		}
	}

	fasthttp.ListenAndServe(fmt.Sprintf(":%d", BidderPort), m)

	evemux := http.NewServeMux()
	evemux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "")
		// log.Println("Event!")
		BidEvent()
	})
	http.ListenAndServe(fmt.Sprintf(":%d", BidderEvent), evemux)

	winmux := http.NewServeMux()
	winmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "")
		// body, _ := ioutil.ReadAll(r.Body)
		// fmt.Println(string(body))
		// log.Println("Win!")
		BidWin()
	})
	http.ListenAndServe(fmt.Sprintf(":%d", BidderWin), winmux)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	select {
	case <-c:
		// Implement remove agent from ACS
		for _, agent := range agents {
			agent.UnregisterAgent(client, ACSIp, ACSPort)
		}
		fmt.Println("Leaving...")
	}
}
