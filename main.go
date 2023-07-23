package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/twilio/twilio-go"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

type Notification struct {
	Receiver string `json:"receiver"`
	Status   string `json:"status"`
	Alerts   []struct {
		Status string `json:"status"`
		Labels struct {
			Alertname string `json:"alertname"`
			Dc        string `json:"dc"`
			Instance  string `json:"instance"`
			Job       string `json:"job"`
		} `json:"labels"`
		Annotations struct {
			Description string `json:"description"`
		} `json:"annotations"`
		StartsAt     time.Time `json:"startsAt"`
		EndsAt       time.Time `json:"endsAt"`
		GeneratorURL string    `json:"generatorURL"`
	} `json:"alerts"`
	GroupLabels  map[string]json.RawMessage `json:"groupLabels"`
	CommonLabels struct {
		Alertname string `json:"alertname"`
		Dc        string `json:"dc"`
		Instance  string `json:"instance"`
		Job       string `json:"job"`
	} `json:"commonLabels"`
	CommonAnnotations struct {
		Description string `json:"description"`
	} `json:"commonAnnotations"`
	ExternalURL string `json:"externalURL"`
	Version     string `json:"version"`
	GroupKey    string `json:"groupKey"`
}

type Alert struct {
	Name     string
	Severity string
}

func getRoot(w http.ResponseWriter, r *http.Request) {
	incidents := r.Context().Value(incidentsStateKey).(*incidentsState)
	tmpl := template.Must(template.ParseFiles("layout.html"))
	log.Printf("[debug] / active incident (%T)-(%p): %v", incidents.Active, incidents.Active, incidents.Active)
	for _, inc := range incidents.List {
		fmt.Printf("Name: %s, Alerts: %v", inc.Name, inc.Alerts)
	}
	tmpl.Execute(w, incidents)
}
func postAlert(w http.ResponseWriter, r *http.Request) {

	queue := r.Context().Value(messageQueueKey).(chan Alert)

	fmt.Printf("got / request\n")
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("could not read body: %s\n", err)
	}
	defer r.Body.Close()

	var notification Notification
	err = json.Unmarshal(body, &notification)

	if err != nil {
		panic(err)
	}
	alertName, _ := strconv.Unquote(string(notification.GroupLabels["alertname"]))
	log.Printf("Alert name: %s is firing\n", alertName)

	alert := Alert{
		Name:     alertName,
		Severity: "critical",
	}
	fmt.Println(alert)
	select {
	case queue <- alert:
		log.Println("[info] Succesfully sent message to queue")
	default:
		log.Println("[warn] queue is full")
	}
}

func getAck(w http.ResponseWriter, r *http.Request) {
	incidents := r.Context().Value(incidentsStateKey).(*incidentsState)

	log.Println("[info] got /ack")
	if incidents.Active != nil {
		log.Printf("[info] /ack active incident (%T)-(%p): %s", incidents, incidents, incidents.Active.Name)
		incidents.ackActive(incidents.Active.Id)
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func getClose(w http.ResponseWriter, r *http.Request) {
	incidents := r.Context().Value(incidentsStateKey).(*incidentsState)

	log.Println("[info] got /close")
	if incidents.Active != nil {
		log.Printf("[info] /close active incident (%T)-(%p): %s", incidents, incidents, incidents.Active.Name)
		incidents.closeActive(incidents.Active.Id)
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

const (
	messageQueueKey   int           = 0
	incidentsStateKey int           = 1
	maxRetries        int           = 3
	retryInterval     time.Duration = 5 * time.Second
)

func makeCall(message string, username string) error {
	boturl, _ := url.Parse("http://api.callmebot.com/start.php")
	params := url.Values{}
	params.Add("source", "openHAB")
	params.Add("user", username)
	params.Add("text", message)

	boturl.RawQuery = params.Encode()

	log.Println("[info] ", boturl.String())

	for i := 0; i < maxRetries; i++ {
		resp, err := http.Get(boturl.String())
		if err != nil {
			log.Println("[error] ", err)
			time.Sleep(retryInterval)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			log.Println("[info] ", resp.Status)
			return nil
		}

		log.Printf("[error] non 200 responce code recived %s\n", resp.Status)
		time.Sleep(retryInterval)
	}
	return fmt.Errorf("maximum number of retries reached")
}
func makePhoneCall(message *string, phone string) error {

	clParams := twilio.ClientParams{
		Username: os.Getenv("TWILLO_USER"),
		Password: os.Getenv("TWILLO_PASS"),
	}

	client := twilio.NewRestClientWithParams(clParams)
	client.SetRegion("")

	params := &api.CreateCallParams{}
	params.SetUrl("http://demo.twilio.com/docs/voice.xml")
	params.SetTo(phone)
	params.SetFrom("+12703723225")
	for i := 1; i < maxRetries; i++ {
		resp, err := client.Api.CreateCall(params)
		if err != nil {
			log.Println("[error] ", err.Error())
			time.Sleep(retryInterval)
			continue
		} else {
			if resp.Sid != nil {
				log.Println("[info] ", *resp.Sid)
			} else {
				log.Println("[info] ", resp.Sid)
			}
			return nil
		}
	}
	return fmt.Errorf("maximum number of retries reached")
}

type telegramMessage struct {
	ChatId int64  `json:"chat_id"`
	Text   string `json:"text"`
}

func sendTelegramMessage(message *string, chatid int64) error {
	tgMessage := telegramMessage{
		ChatId: chatid,
		Text:   *message,
	}
	tgBotToken := os.Getenv("TG_BOT_TOKEN")
	url := "https://api.telegram.org/bot" + tgBotToken + "/sendMessage"
	payload, err := json.Marshal(tgMessage)
	if err != nil {
		log.Println("[error] cannot marshal tg message ", err)
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		log.Println("[error] ", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Println("[info] ", resp.Status)
		return nil
	} else {
		log.Printf("[info] non 200 responce code recived %s\n", resp.Status)
		return errors.New("non 200 responce code recived")
	}

}

type incident struct {
	Id       string
	Name     string
	Status   string
	StartsAt time.Time
	EndsAt   time.Time
	Severity string
	Alerts   []Alert
	Actions  []*Action
}

type Action struct {
	Channel string
	Time    time.Time
	Message string
}

type incidentsState struct {
	Active      *incident
	List        []*incident
	EventsQueue chan *incident
}

func (i *incidentsState) createNew(name string, severity string) {
	randomInt := rand.Uint64()

	new_incident := incident{
		Id:       fmt.Sprintf("%x", randomInt),
		Name:     name,
		Status:   "new",
		StartsAt: time.Now(),
		Severity: severity,
		Alerts:   []Alert{{name, severity}},
	}
	i.List = append(i.List, &new_incident)
	i.Active = &new_incident
	log.Println("[info] new incident is ", i.Active)
	i.EventsQueue <- &new_incident
	log.Printf("[info] incident created")
}

func (i *incidentsState) ackActive(id string) error {
	if i.Active.Id != id {
		return errors.New(fmt.Sprintf("id of active incedent: %s, ack for id recived is: %s", i.Active.Id, id))
	}
	log.Printf("[debug] going to ack inc: (%T-%p) in a state: (%T-%p)", i.Active, i.Active, i, i)
	i.Active.Status = "ack"
	return nil
}

func (i *incidentsState) closeActive(id string) error {
	if i.Active.Id != id {
		return errors.New(fmt.Sprintf("id of active incedent: %s, close for id recived is: %s", i.Active.Id, id))
	}
	i.Active.Status = "closed"
	i.Active.EndsAt = time.Now()
	i.Active = nil
	return nil
}

func incidentWatcher(queue <-chan *incident) {
	log.Println("[info] incidentWatcher started")
	for inc := range queue {
		log.Printf("[event] incidend event recived id: %s name: %s started: %s", inc.Id, inc.Name, inc.StartsAt)
		go incidentNotifyer(inc)
	}
}

func incidentNotifyer(incident *incident) {

	for incident.Status == "new" {
		tg_action := Action{
			Channel: "telegram",
			Time:    time.Now(),
			Message: fmt.Sprintf("New Incident\n Name: %s\n Severity: %s", incident.Name, incident.Severity),
		}

		err := sendTelegramMessage(&tg_action.Message, -984782066)

		if err != nil {
			log.Println("[error] send tg message: ", err)
		} else {
			incident.Actions = append(incident.Actions, &tg_action)
		}

		phone_action := Action{
			Channel: "phone",
			Time:    time.Now(),
			Message: "no message",
		}

		err = makePhoneCall(&tg_action.Message, "+79251893906")

		if err != nil {
			log.Println("[error] make a phone call ", err)
		} else {
			incident.Actions = append(incident.Actions, &phone_action)
		}
		time.Sleep(60 * time.Second)
	}
}

func proccessingIncidents(incidents *incidentsState) {
	// for {
	// 	inc := incidents.Active
	// 	if inc != nil {
	// 		log.Printf("[info] have active incident %p is: %s status: %s alerts: %v", inc, inc.Name, inc.Status, inc.Alerts)

	// 		if inc.Status == "new" {

	// 			tg_action := Action{
	// 				Channel: "telegram",
	// 				Time:    time.Now(),
	// 				Message: fmt.Sprintf("New Incident\n Name: %s\n Severity: %s", inc.Name, inc.Severity),
	// 			}

	// 			err := sendTelegramMessage(&tg_action.Message, -984782066)

	// 			if err != nil {
	// 				log.Println("[error] send tg message: ", err)
	// 			} else {
	// 				inc.Actions = append(inc.Actions, &tg_action)
	// 			}

	// 			phone_action := Action{
	// 				Channel: "phone",
	// 				Time:    time.Now(),
	// 				Message: "no message",
	// 			}

	// 			err = makePhoneCall(&tg_action.Message, "+79251893906")

	// 			if err != nil {
	// 				log.Println("[error] make a phone call ", err)
	// 			} else {
	// 				inc.Actions = append(inc.Actions, &phone_action)
	// 			}
	// 		}

	// 	} else {
	// 		log.Println("[info] no active incident")
	// 	}
	// 	time.Sleep(60 * time.Second)
	// }
}

func proccessingMessages(incidents *incidentsState, queue <-chan Alert) {
	for alert := range queue {
		log.Printf("recive: %s\n", alert)
		//err := makeCall(message, "@fbbbbbbw")
		//err := makePhoneCall(message, "+79251893906")
		//err := sendTelegramMessage(&message, -984782066)

		if incidents.Active != nil {
			incidents.Active.Alerts = append(incidents.Active.Alerts, alert)
			log.Printf("[info] active incident is %s with alerts %v: ", incidents.Active.Name, incidents.Active.Alerts)
		} else {
			log.Println("[info] no active incident")
			incidents.createNew(alert.Name, alert.Severity)
			log.Println("[info] incident created succses")
		}

		// if err != nil {
		// 	log.Println("[error] cannot process message: ", err)
		// }
		//time.Sleep(15 * time.Second)
	}
}
func main() {

	incidents := incidentsState{
		Active:      nil,
		List:        make([]*incident, 0),
		EventsQueue: make(chan *incident),
	}
	log.Printf("[debug] created (%T - %p)", &incidents, &incidents)
	alertQueue := make(chan Alert, 3)
	go proccessingMessages(&incidents, alertQueue)
	go incidentWatcher(incidents.EventsQueue)
	//go proccessingIncidents(&incidents)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), incidentsStateKey, &incidents)
		getRoot(w, r.WithContext(ctx))

	})
	http.HandleFunc("/alert", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), messageQueueKey, alertQueue)
		postAlert(w, r.WithContext(ctx))

	})
	http.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[debug] put (%T - %p) in ack handle", &incidents, &incidents)
		ctx := context.WithValue(r.Context(), incidentsStateKey, &incidents)
		getAck(w, r.WithContext(ctx))

	})
	http.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[debug] put (%T - %p) in ack handle", &incidents, &incidents)
		ctx := context.WithValue(r.Context(), incidentsStateKey, &incidents)
		getClose(w, r.WithContext(ctx))

	})
	err := http.ListenAndServe(":3333", nil)
	if err != nil {
		fmt.Printf("can't start server: %s\n", err)
		os.Exit(1)
	}
}
