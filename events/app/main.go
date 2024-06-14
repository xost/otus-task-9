package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
)

type eventModel struct {
	ID         int    `json:"id,omitempty"`
	Name       string `json:"event_name"`
	Price      int    `json:"price"`
	TotalSlots int    `json:"total_slots"`
}

type occupyRequestModel struct {
	BookID  int `json:"book_id"`
	EventID int `json:"event_id"`
}

type occupiedResponseModel struct {
	BookID int  `json:"book_id"`
	UserID int  `json:"user_id"`
	Price  int  `json:"price"`
	Status bool `json:"status"`
}

type configModel struct {
	dbHost string
	dbPort string
	dbName string
	dbUser string
	dbPass string
	host   string
	port   string
}

const (
	statusCreated = iota
	statusOccupied
	StatusPaid
	statusCommited
	statusCancelled = -1
)

const (
	createEventTpl       = `INSERT INTO events (event_name, price, total_slots) VALUES ($1, $2, $3)`
	occupySlotTpl        = `INSERT INTO slots (event_id, book_id) VALUES ($1, $2)`
	cancelSlotTpl        = `DELETE FROM slots WHERE book_id = $1`
	occupiedSlotsTpl     = `SELECT COUNT(1) FROM slots WHERE event_id=$1`
	getEventTpl          = `SELECT id, event_name, price, total_slots FROM events WHERE id=$1`
	getEventsTpl         = `SELECT id, event_name, price, total_slots FROM events`
	bookCallbackEndpoint = "http://book.saga.svc.cluster.local:9000/book/callback/events"
)

var (
	createEventStmt   *sql.Stmt
	occupySlotStmt    *sql.Stmt
	cancelSlotStmt    *sql.Stmt
	occupiedSlotsStmt *sql.Stmt
	getEventStmt      *sql.Stmt
	getEventsStmt     *sql.Stmt
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "",
		dbPort: "5432",
		dbName: "",
		dbUser: "",
		dbPass: "",
		host:   "0.0.0.0",
		port:   "80",
	}
	dbHost := os.Getenv("DBHOST")
	dbPort := os.Getenv("DBPORT")
	dbName := os.Getenv("DBNAME")
	dbUser := os.Getenv("DBUSER")
	dbPass := os.Getenv("DBPASS")
	host := os.Getenv("HOST")
	port := os.Getenv("PORT")

	if dbHost != "" {
		cfg.dbHost = dbHost
	}
	if dbPort != "" {
		cfg.dbPort = dbPort
	}
	if dbName != "" {
		cfg.dbName = dbName
	}
	if dbUser != "" {
		cfg.dbUser = dbUser
	}
	if dbPass != "" {
		cfg.dbPass = dbPass
	}
	if host != "" {
		cfg.host = host
	}
	if port != "" {
		cfg.port = port
	}
	return cfg
}

func makeDBConn(cfg *configModel) (*sql.DB, error) {
	pgConnString := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.dbHost, cfg.dbPort, cfg.dbUser, cfg.dbPass, cfg.dbName,
	)
	log.Println("connection string: ", pgConnString)
	db, err := sql.Open("postgres", pgConnString)
	return db, err
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := readConf()

	db, err := makeDBConn(cfg)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer db.Close()

	if err = db.PingContext(ctx); err != nil {
		log.Fatal("Failed to check db connection:", err)
	}

	mustPrepareStmts(ctx, db)

	r := mux.NewRouter()

	r.HandleFunc("/events/create", reqlog(isAuthenticatedMiddleware(create))).Methods("POST")
	r.HandleFunc("/events/get", reqlog(isAuthenticatedMiddleware(get))).Methods("GET")
	r.HandleFunc("/events/get/{id}", reqlog(isAuthenticatedMiddleware(get))).Methods("GET")
	r.HandleFunc("/events/occupy", reqlog(isAuthenticatedMiddleware(occupy))).Methods("POST")
	r.HandleFunc("/events/cancel", reqlog(isAuthenticatedMiddleware(cancelSlot))).Methods("POST")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createEventStmt, err = db.PrepareContext(ctx, createEventTpl)
	if err != nil {
		panic(err)
	}

	occupySlotStmt, err = db.PrepareContext(ctx, occupySlotTpl)
	if err != nil {
		panic(err)
	}

	cancelSlotStmt, err = db.PrepareContext(ctx, cancelSlotTpl)
	if err != nil {
		panic(err)
	}

	occupiedSlotsStmt, err = db.PrepareContext(ctx, occupiedSlotsTpl)
	if err != nil {
		panic(err)
	}

	getEventStmt, err = db.PrepareContext(ctx, getEventTpl)
	if err != nil {
		panic(err)
	}
	getEventsStmt, err = db.PrepareContext(ctx, getEventsTpl)
	if err != nil {
		panic(err)
	}
}

func createEvent(name string, price, totalSlots int) error {
	_, err := createEventStmt.Exec(name, price, totalSlots)
	if err != nil {
		log.Printf("Failed to create event with name [%s]: %s", name, err)
		return err
	}
	return nil
}

func create(w http.ResponseWriter, r *http.Request) {
	e := eventModel{}
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if err := createEvent(e.Name, e.Price, e.TotalSlots); err != nil {
		log.Printf("Failed to create event with name [%s] price [%d] slots [%d]: %s\n", e.Name, e.Price, e.TotalSlots, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully created event with name [%s] price [%d] slots [%d]\n", e.Name, e.Price, e.TotalSlots)
	w.WriteHeader(http.StatusOK)
}

func getTotalSlots(id int) int {
	e, err := getEvent(id)
	if err != nil {
		log.Printf("Failed to get event id [%d]: %s\n", id, err)
		return 0
	}
	return e.TotalSlots
}

func getOccupiedSlots(id int) int {
	row := occupiedSlotsStmt.QueryRow(id)
	occ := new(int)
	if err := row.Scan(&occ); err != nil {
		log.Printf("Failed to get occupied slots for event id [%d]:%s\n", id, err)
		return 0
	}
	return *occ
}

func getEvent(id int) (*eventModel, error) {
	row := getEventStmt.QueryRow(id)
	e := &eventModel{ID: id}
	err := row.Scan(&e.ID, &e.Name, &e.Price, &e.TotalSlots)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func getEvents() ([]eventModel, error) {
	rows, err := getEventsStmt.Query()
	if err != nil {
		return nil, err
	}
	es := []eventModel{}
	e := eventModel{}
	for rows.Next() {
		err := rows.Scan(&e.ID, &e.Name, &e.Price, &e.TotalSlots)
		if err != nil {
			log.Printf("Failed to get values: %s", err)
			break
		}
		es = append(es, e)
	}
	return es, nil
}

func get(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	if id_, ok := vars["id"]; ok {
		id, err := strconv.Atoi(id_)
		if err != nil {
			log.Println("Failed to parse request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e, err := getEvent(id)
		if err != nil {
			log.Printf("Could not find any event with id [%d]\n", id)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := json.Marshal(e)
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}
	es, err := getEvents()
	if err != nil {
		log.Printf("Failed to get event's list: %s", err)
	}
	data, _ := json.Marshal(es)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func occupySlot(eid, oid int) error {
	_, err := occupySlotStmt.Exec(eid, oid)
	return err
}

func occupy(w http.ResponseWriter, r *http.Request) {
	uid, err := getUserID(r)
	if err != nil {
		log.Printf("Failed to get User ID: %s", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	o := occupyRequestModel{}
	if err = json.NewDecoder(r.Body).Decode(&o); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	ro := &occupiedResponseModel{
		BookID: o.BookID,
		UserID: uid,
		Status: false,
	}
	e := &eventModel{}
	if e, err = getEvent(o.EventID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to get event [%d]: %s\n", o.EventID, err)
		sendCallback(ro)
		return
	}
	ro.Price = e.Price
	total := getTotalSlots(o.EventID)
	occupied := getOccupiedSlots(o.EventID)
	if total > occupied {
		if err = occupySlot(o.EventID, o.BookID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			sendCallback(ro)
			log.Printf("Failed to occupy slot on events [%d] for book [%d]: %s\n", o.EventID, o.BookID, err)
			return
		}
	} else {
		w.WriteHeader(http.StatusOK)
		log.Println("Slot was not occupied due to there is no available slots any more")
		sendCallback(ro)
		return
	}
	log.Println("Slot was occupied successfully, send callback to book service")
	w.WriteHeader(http.StatusOK)
	ro.Status = true
	sendCallback(ro)
}

func cancelSlot(w http.ResponseWriter, r *http.Request) {
	o := occupyRequestModel{}
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if _, err := cancelSlotStmt.Exec(o.BookID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Failed to cancel slot occuping:", err)
	}
}

func sendCallback(r *occupiedResponseModel) {
	data, err := json.Marshal(r)
	if err != nil {
		log.Printf("Failed to parse data: %s\n", err)
		return
	}
	reqBody := bytes.NewReader(data)
	req, err := http.NewRequest("POST", bookCallbackEndpoint, reqBody)
	if err != nil {
		log.Printf("Failed callback request: %s\n", err)
		return
	}
	req.Header.Set("X-User-Id", strconv.Itoa(r.UserID))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("Failed to call back book endpoint: %s\n", err)
		return
	}
	defer resp.Body.Close()
}

func isAuthenticatedMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header
		if _, ok := headers["X-User-Id"]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Not authenticated"))
			return
		}
		h.ServeHTTP(w, r)
	}
}

func reqlog(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Got request from: %s\n", r.Host)
		h.ServeHTTP(w, r)
	}
}

func getUserID(r *http.Request) (int, error) {
	return strconv.Atoi(r.Header.Get("X-User-Id"))
}
