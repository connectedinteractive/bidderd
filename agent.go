package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	openrtb "gopkg.in/bsm/openrtb.v2"
)

const initialCapacity = 25 // No special reason why it's 25.

type Creative struct {
	Format string `json:"format"`
	Id     int    `json:"id"`
	Name   string `json:"name"`
}

// This is the agent configuration that will be sent to RTBKIT's ACS
type AgentConfig struct {
	// We use `RawMessage` for Augmentations and BidcControl, because we
	// don't need it, we just cache it.
	Account            []string         `json:"account"`
	Augmentations      *json.RawMessage `json:"augmentations"`
	BidControl         *json.RawMessage `json:"bidControl"`
	BidProbability     float64          `json:"bidProbability"`
	Creatives          []Creative       `json:"creatives"`
	ErrorFormat        string           `json:"errorFormat"`
	External           bool             `json:"external"`
	ExternalId         int              `json:"externalId"`
	LossFormat         string           `json:"lossFormat"`
	MinTimeAvailableMs float64          `json:"minTimeAvailableMs"`
	WinFormat          string           `json:"winFormat"`
}

// This represents a RTBKIT Agent
type Agent struct {
	Name   string      `json:"name"`
	Config AgentConfig `json:"config"`

	// This is the price the agent will pay per impression. "Fixed price bidder".
	Price float64 `json:"price"`

	// For pacing the budgeting
	Period  int `json:"period"`
	Balance int `json:"balance"`

	// private state of each agent
	registered bool      // did we register the configuration in the ACS?
	pacer      chan bool // go routine updating balance in the banker
	bidId      int       // unique id for response
}

type creativesKey struct {
	// This is used to make a mapping between an impression and the
	// external-id of an agent to the creatives that can be sent to the
	// exchange for that impression.
	ImpId string
	ExtId int
}

// Register Agent in the ACS sending a HTTP request to the service on `acsIp`:`acsPort`
func (agent *Agent) RegisterAgent(
	httpClient *http.Client, acsIp string, acsPort int) {
	url := fmt.Sprintf("http://%s:%d/v1/agents/%s/config", acsIp, acsPort, agent.Name)
	body, _ := json.Marshal(agent.Config)
	reader := bytes.NewReader(body)
	req, _ := http.NewRequest("POST", url, reader)
	req.Header.Add("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		log.Printf("ACS registration failed with %s\n", err)
		return
	}
	agent.registered = true
	res.Body.Close()
}

// Removes the agent configuration from the ACS
func (agent *Agent) UnregisterAgent(
	httpClient *http.Client, acsIp string, acsPort int) {
	url := fmt.Sprintf("http://%s:%d/v1/agents/%s/config", acsIp, acsPort, agent.Name)
	req, _ := http.NewRequest("DELETE", url, bytes.NewBufferString(""))
	res, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Unregister failed with %s\n", err)
		return
	}
	agent.registered = false
	res.Body.Close()
}

// Starts a go routine which periodically updates the balance on the agents account.
func (agent *Agent) StartPacer(
	httpClient *http.Client, bankerIp string, bankerPort int) {

	accounts := agent.Config.Account

	url := fmt.Sprintf("http://%s:%d/v1/accounts/%s/balance",
		bankerIp, bankerPort, strings.Join(accounts, ":"))
	body := fmt.Sprintf("{\"USD/1M\": %d}", agent.Balance)
	ticker := time.NewTicker(time.Duration(agent.Period) * time.Millisecond)
	agent.pacer = make(chan bool)

	go func() {
		for {
			select {
			case <-ticker.C:
				// make this a new go routine?
				go func() {
					log.Println("Pacing...")
					req, _ := http.NewRequest("POST", url, strings.NewReader(body))
					req.Header.Add("Accept", "application/json")
					res, err := httpClient.Do(req)
					if err != nil {
						log.Printf("Balance failed with %s\n", err)
						return
					}
					res.Body.Close()
				}()
			case <-agent.pacer:
				ticker.Stop()
				return
			}
		}
	}()
}

// Stops the go routine updating the bank balance.
func (agent *Agent) StopPacer() {
	close(agent.pacer)
}

// Adds to the bid response the bid by the agent. The Bid is added to
// the only seat of the response. It picks a random creative from
// the list of creatives from the `Agent.Config.Creative` and places it
// in the bid.
func (agent *Agent) DoBid(
	req *openrtb.BidRequest, res *openrtb.BidResponse, ids map[creativesKey]interface{}) (*openrtb.BidResponse, bool) {

	for _, imp := range req.Imp {
		key := creativesKey{ImpId: imp.ID, ExtId: agent.Config.ExternalId}
		if ids[key] == nil {
			continue
		}
		creativeList := ids[key].([]interface{})
		// pick a random creative
		n := rand.Intn(len(creativeList))

		// JSON reads numbers as float64...
		cridx := int(creativeList[n].(float64))
		// ...but this (`cridx` see below) is an index.
		creative := agent.Config.Creatives[cridx]
		crid := strconv.Itoa(creative.Id)

		// the `bidId` should be something else,
		// it is used for tracking the bid,
		// but we are not tracking anything yet.
		bidId := strconv.Itoa(agent.bidId)

		price := float64(agent.Price)

		ext := map[string]interface{}{"priority": 1.0, "external-id": agent.Config.ExternalId}
		jsonExt, _ := json.Marshal(ext)
		bid := openrtb.Bid{ID: bidId, ImpID: imp.ID, CreativeID: crid, Price: price, Ext: jsonExt}
		agent.bidId += 1
		res.SeatBid[0].Bid = append(res.SeatBid[0].Bid, bid)
	}
	return res, len(res.SeatBid[0].Bid) > 0
}

func externalIdsFromRequest(req *openrtb.BidRequest) map[creativesKey]interface{} {
	// This function makes a mappping with a range of type (Impression Id, External Id)
	// to a slice of "creative indexes" (See the agent configuration "creative").
	// We use this auxiliary function in `DoBid` to match the `BidRequest` to the
	// creatives of the agent and create a response.
	ids := make(map[creativesKey]interface{})

	for _, imp := range req.Imp {
		for _, extID := range imp.Ext["external-ids"].([]interface{}) {
			key := creativesKey{ImpId: imp.ID, ExtId: int(extID.(float64))}
			creatives := (imp.Ext["creative-ids"].(map[string]interface{}))[strconv.Itoa(int(extID.(float64)))]
			ids[key] = creatives.(interface{})
		}
	}
	return ids
}

func emptyResponseWithOneSeat(req *openrtb.BidRequest) *openrtb.BidResponse {
	// This function adds a Seat to the Response.
	// Seat: A buyer entity that uses a Bidder to obtain impressions on its behalf.
	seat := openrtb.SeatBid{Bid: make([]openrtb.Bid, 0)}
	seatbid := []openrtb.SeatBid{seat}
	res := &openrtb.BidResponse{ID: req.ID, SeatBid: seatbid}
	return res
}

// Parse a JSON file and return an Agent.
func LoadAgent(filepath string) (Agent, error) {
	var agent Agent
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return Agent{}, err
	}
	err = json.Unmarshal(data, &agent)
	if err != nil {
		return Agent{}, err
	}
	return agent, nil
}

// Parse a JSON file and return a list of Agents.
func LoadAgentsFromFile(filepath string) ([]Agent, error) {
	type Agents []Agent
	var agents Agents

	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return agents, err
	}
	err = json.Unmarshal(data, &agents)
	if err != nil {
		return agents, err
	}
	return agents, nil
}
