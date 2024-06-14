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

type orderModel struct {
	Item   string `json:"item"`
	Amount int    `json:"amount"`
}

type balanceModel struct {
	Balance int `json:"balance"`
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
	createOrderTpl = `INSERT INTO orders (userid, item, amount) VALUES ($1, $2, $3) returning id`
	notifTpl       = `{"userid":%d,"message":"%s"}`
)

var (
	createOrderStmt *sql.Stmt
)

func readConf() *configModel {
	cfg := &configModel{
		dbHost: "orders-postgresql",
		dbPort: "5432",
		dbName: "ordersdb",
		dbUser: "ordersuser",
		dbPass: "orderspasswd",
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

	r.HandleFunc("/orders/create", isAuthenticatedMiddleware(create)).Methods("POST")

	bindOn := fmt.Sprintf("%s:%s", cfg.host, cfg.port)
	if err := http.ListenAndServe(bindOn, r); err != nil {
		log.Printf("Failed to bind on [%s]: %s", bindOn, err)
	}
}

func mustPrepareStmts(ctx context.Context, db *sql.DB) {
	var err error

	createOrderStmt, err = db.PrepareContext(ctx, createOrderTpl)
	if err != nil {
		panic(err)
	}

}

func createOrder(id, amount int, item string) error {
	_, err := createOrderStmt.Query(id, item, amount)
	if err != nil {
		log.Printf("Failed to create order for user id [%d]: %s", id, err)
		return err
	}
	return nil
}

func createNotif(id int, message string) error {
	b := bytes.NewReader([]byte(fmt.Sprintf(notifTpl, id, message)))
	req, err := http.NewRequest("POST", "http://notif.saga.svc.cluster.local:9000/notif/create", b)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Id", strconv.Itoa(id))
	c := http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("Failed to execute request to get balance: %s\n", err)
		return err
	}
	defer resp.Body.Close()
	return nil
}

// func getbalance(id int) (int, error) {
// 	req, err := http.NewRequest("GET", "http://account.saga.svc.cluster.local:9000/account/get", nil)
// 	if err != nil {
// 		return 0, err
// 	}
// 	req.Header.Set("X-User-Id", strconv.Itoa(id))
// 	c := http.Client{}
// 	resp, err := c.Do(req)
// 	if err != nil {
// 		log.Printf("Failed to execute request to get balance: %s\n", err)
// 		return 0, err
// 	}
// 	defer resp.Body.Close()
// 	data, err := io.ReadAll(resp.Body)
// 	if err != nil {
// 		return 0, nil
// 	}
// 	b := balanceModel{}
// 	if err = json.Unmarshal(data, &b); err != nil {
// 		log.Printf("Failed parse response: %s\n\t request to get balance: %s\n", string(data), err)
// 		return 0, err
// 	}
// 	return b.Balance, nil
// }
//
// func deposit(id, amount int) error {
// 	b := bytes.NewReader([]byte(fmt.Sprintf(`{"id":%d,"delta":%d}`, id, amount)))
// 	req, err := http.NewRequest("PUT", "http://account.saga.svc.cluster.local:9000/account/deposit", b)
// 	if err != nil {
// 		return err
// 	}
// 	req.Header.Set("X-User-Id", strconv.Itoa(id))
// 	c := http.Client{}
// 	resp, err := c.Do(req)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusOK {
// 		return fmt.Errorf("failed to withdrawal fund for user %d", id)
// 	}
// 	return nil
// }

func create(w http.ResponseWriter, r *http.Request) {
	headers := r.Header
	id, err := strconv.Atoi(headers.Get("X-User-Id"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Got wrong header [X-User-Id]: %s", err)
		return
	}
	o := orderModel{}
	if err = json.NewDecoder(r.Body).Decode(&o); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("Failed to parse request body user id [%d]: %s\n", id, err)
		return
	}
	if err = createOrder(id, o.Amount, o.Item); err != nil {
    }
		w.WriteHeader(http.StatusInternalServerError)
		if err = createNotif(id, "Failed to create order. Your funds will be return on your account"); err != nil {
			log.Printf("Failed to create notification for user id [%d]: %s\n", id, err)
		}
		return
	}
	if err = createNotif(id, fmt.Sprintf("Successfully created order with %s", o.Item)); err != nil {
		log.Printf("Failed to create notification for user id [%d]: %s\n", id, err)
	}
	log.Printf("Successfully created order for user id [%d]\n", id)
	w.WriteHeader(http.StatusOK)
}

func isAuthenticatedMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header
		fmt.Println(headers)
		if _, ok := headers["X-User-Id"]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Not authenticated"))
			return
		}
		h.ServeHTTP(w, r)
	}
}
