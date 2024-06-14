package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
)

type bookModel struct {
	ID      int `json:"id"`
	UserID  int `json:"user_id"`
	EventID int `json:"event_id"`
	Price   int `json:"price,omitempty"`
	Status  int `json:"status,omitempty"`
}

type callbackOccupyModel struct {
	BookID int  `json:"book_id"`
	UserID int  `json:"user_id"`
	Price  int  `json:"price"`
	Status bool `json:"status"`
}

type callbackPaymentModel struct {
	BookID int  `json:"book_id"`
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
	statusNeedToOccupy
	statusOccupied
	statusNeedToPay
	StatusPaid
	StatusNeetToNotify
	statusCompleted
	statusCancelled = -1
)

const (
	createBookTpl       = `INSERT INTO book (user_id, event_id, price, status) VALUES ($1, $2, 0,0) returning id`
	updateStatusTpl     = `UPDATE book SET status=$2 WHERE id=$1`
	setPriceTpl         = `UPDATE book SET price=$2 WHERE id=$1`
	getBookTpl          = `SELECT id, user_id, event_id, price, status FROM book WHERE id=$1`
	getBooksTpl         = `SELECT id, user_id, event_id, price, status FROM book`
	occupySlotEndpoint  = "http://events.saga.svc.cluster.local:9000/events/occupy"
	cancelSlotEndpoint  = "http://events.saga.svc.cluster.local:9000/events/cancel"
	paymentSlotEndpoint = "http://account.saga.svc.cluster.local:9000/account/withdrawal"
	occupySlotTpl       = `{"book_id":%d,"event_id":%d}`
	payTpl              = `{"book_id":%d,"withdrawal_sum":%d}`
)

var (
	createBookStmt   *sql.Stmt
	updateStatusStmt *sql.Stmt
	setPriceStmt     *sql.Stmt
	getStatusStmt    *sql.Stmt
	getBookStmt      *sql.Stmt
	getBooksStmt     *sql.Stmt
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

	r.HandleFunc("/book/get", reqlog(isAuthenticatedMiddleware(get))).Methods("GET")
	r.HandleFunc("/book/create", reqlog(isAuthenticatedMiddleware(create))).Methods("POST")
	r.HandleFunc("/book/callback/events", reqlog(isAuthenticatedMiddleware(callbackEvents))).Methods("POST")
	r.HandleFunc("/book/callback/account", reqlog(isAuthenticatedMiddleware(callbackPayment))).Methods("POST")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createBookStmt, err = db.PrepareContext(ctx, createBookTpl)
	if err != nil {
		panic(err)
	}

	updateStatusStmt, err = db.PrepareContext(ctx, updateStatusTpl)
	if err != nil {
		panic(err)
	}

	setPriceStmt, err = db.PrepareContext(ctx, setPriceTpl)
	if err != nil {
		panic(err)
	}

	getBookStmt, err = db.PrepareContext(ctx, getBookTpl)
	if err != nil {
		panic(err)
	}
	getBooksStmt, err = db.PrepareContext(ctx, getBooksTpl)
	if err != nil {
		panic(err)
	}

}

func book(userID int, b *bookModel) (int, error) {
	id := new(int)
	err := createBookStmt.QueryRow(userID, b.EventID).Scan(id)
	return *id, err
}

func getBook(bid int) (*bookModel, error) {
	b := bookModel{}
	err := getBookStmt.QueryRow(bid).Scan(&b.ID, &b.UserID, &b.EventID, &b.Price, &b.Status)
	return &b, err
}

func cancelBook(bid int) error {
	_, err := updateStatusStmt.Exec(bid, statusCancelled)
	return err
}

func modifyBookStatus(bid, status int) error {
	_, err := updateStatusStmt.Exec(bid, status)
	return err
}

func setBookPrice(bid, price int) error {
	_, err := setPriceStmt.Exec(bid, price)
	return err
}

func actionBookStatus(bid int) error {
	b, err := getBook(bid)
	if err != nil {
		log.Printf("Failed to get book [%d]: %s\n", bid, err)
		return err
	}
	switch b.Status {
	case statusCreated:
		log.Println("Book is created, now we need to occupy the slot")
		modifyBookStatus(bid, statusNeedToOccupy)
		if err = actionBookStatus(bid); err != nil {
			if err = cancelBook(b.ID); err != nil {
				log.Printf("Failed to cancel book [%d]\n", b.ID)
			}
			log.Printf("Failed to perform action for book [%d] with status [%d]:%s\n", bid, statusNeedToOccupy, err)
		}
	case statusCancelled:
		log.Println("Book is canceled, do nothing")
	case statusNeedToOccupy:
		log.Printf("Book [%d] is created, now need to occupy slot\n", b.ID)
		if err = occupySlot(b.ID, b.EventID, b.UserID); err != nil {
			log.Printf("Failed to occupy slot for event [%d] for user [%d], need to cancel book. Error: %s\n", b.EventID, b.UserID, err)
			if err = cancelBook(b.ID); err != nil {
				log.Printf("Failed to cancel book [%d]\n", b.ID)
			}
		}
	case statusOccupied:
		log.Println("Slot is occupied, now we need to pay for book")
		modifyBookStatus(bid, statusNeedToPay)
		if err = actionBookStatus(bid); err != nil {
			if err = cancelBook(b.ID); err != nil {
				log.Printf("Failed to cancel book [%d]\n", b.ID)
			}
			log.Printf("Failed to perform action for book [%d] with status [%d]:%s\n", bid, statusNeedToOccupy, err)
		}
	case statusNeedToPay:
		log.Println("Event's slot is occupied, so we need to pay for event")
		if err = payForBook(b); err != nil { // i need to know price for event, so i have to get it from events service
			log.Printf("Failed to pay the for event [%d] for user [%d], need to cancel book\n", b.EventID, b.UserID)
			// also we have to cancel slot, but not now
			if err = cancelBook(b.ID); err != nil {
				log.Printf("Failed to cancel book [%d]: %s\n", b.ID, err)
			}
			if err = cancelSlot(b); err != nil {
				log.Printf("Failed to cancel slot [%d]: %s\n", b.ID, err)
			}
		}
	case StatusPaid:
		log.Println("Event's slot is paid, so the book is complete")
		// need to notify here
	default:
		log.Println("This should not be happen never")
	}
	return err
}

func get(w http.ResponseWriter, r *http.Request) {
	// uid, err := getUserID(r)
	// if err != nil {
	// 	log.Printf("Failed to get user id:", err)
	// 	w.WriteHeader(http.StatusInternalServerError)
	// 	return
	// }
	// id, user_id, event_id, price, status
	rows, err := getBooksStmt.Query()
	if err != nil {
		log.Printf("Failed to get books list: %s\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	id := new(int)
	user_id := new(int)
	event_id := new(int)
	price := new(int)
	status := new(int)
	books := make([]bookModel, 0)
	for rows.Next() {
		err := rows.Scan(id, user_id, event_id, price, status)
		if err != nil {
			log.Println("Failed to scan current row:", err)
		}
		books = append(books, bookModel{
			ID:      *id,
			UserID:  *user_id,
			EventID: *event_id,
			Price:   *price,
			Status:  *status,
		})
	}
	data, _ := json.Marshal(books)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func create(w http.ResponseWriter, r *http.Request) {
	headers := r.Header
	userID, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	b := bookModel{}
	if err = json.NewDecoder(r.Body).Decode(&b); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	id, err := book(userID, &b)
	if err != nil {
		log.Printf("Failed to book event [%d] for user [%d]: %s\n", b.EventID, userID, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully booked events [%d] for user [%d]\n", b.EventID, userID)
	w.WriteHeader(http.StatusOK)
	if err = actionBookStatus(id); err != nil {
		log.Printf("Failed to perform action based on book's status: %s\n", err)
	}
}

func occupySlot(bid, eid, uid int) error {
	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(occupySlotTpl, bid, eid)))
	req, err := http.NewRequest(http.MethodPost, occupySlotEndpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Id", strconv.Itoa(uid))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to occupy slot")
	}
	return nil
}

func payForBook(b *bookModel) error {
	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(payTpl, b.ID, b.UserID, b.Price)))
	req, err := http.NewRequest(http.MethodPut, paymentSlotEndpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Id", strconv.Itoa(b.UserID))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to pay for book")
	}
	return nil
}

func cancelSlot(b *bookModel) error {
	bodyReader := bytes.NewReader([]byte(fmt.Sprintf(occupySlotTpl, b.ID, b.EventID)))
	req, err := http.NewRequest(http.MethodPost, occupySlotEndpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Id", strconv.Itoa(b.UserID))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to cancel slot")
	}
	return nil
}

func callbackEvents(w http.ResponseWriter, r *http.Request) {
	c := callbackOccupyModel{}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if c.Status {
		if err := modifyBookStatus(c.BookID, statusOccupied); err != nil {
			log.Printf("Failed to set book price:%s\n", err)
		}
		if err := setBookPrice(c.BookID, c.Price); err != nil {
			log.Printf("Failed to set book price:%s Cancel the book\n", err)
			_ = modifyBookStatus(c.BookID, statusCancelled)
		}
		if err := actionBookStatus(c.BookID); err != nil {
			log.Printf("Failed to action for current book's status\n")
		}
		return
	}
	log.Printf("Failed to occupy event's slot, book will canceled")
	if err := cancelBook(c.BookID); err != nil {
		log.Printf("Failed to cancel book [%d]\n", c.BookID)
	}
}

func callbackPayment(w http.ResponseWriter, r *http.Request) {
	c := callbackPaymentModel{}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id []: %s\n", err)
		return
	}
	if c.Status {
		modifyBookStatus(c.BookID, StatusPaid)
		if err := actionBookStatus(c.BookID); err != nil {
			log.Printf("Failed to action for current book's status\n")
		}
		return
	}
	log.Printf("Failed to pay event's slot, book will canceled")
	if err := cancelBook(c.BookID); err != nil {
		log.Printf("Failed to cancel book [%d]\n", c.BookID)
	}
}

func isAuthenticatedMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header
		if _, ok := headers["X-User-Id"]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Not authenticated"))
			log.Println("Not authenticated")
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
