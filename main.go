package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	_ "github.com/mattn/go-sqlite3"
)

type UserEntry struct {
	UserID   int    `json:"userid" binding:"required,numeric"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

type LoginResponse struct {
	AuthToken    string `json:"authtoken" binding:"required,jwt"`
	RefreshToken string `json:"refreshtoken" binding:"required,jwt"`
}

var DB *sql.DB
var port int
var datapath string

func init() {
	cfg, err := ini.Load("settings.ini")
	if err != nil {
		fmt.Printf("Fail to read file: %v", err)
		os.Exit(1)
	}

	port = cfg.Section("server").Key("port").MustInt(8080)
	datapath = cfg.Section("paths").Key("datapath").MustString("./")

	path, err := filepath.Abs(datapath)
	if err != nil {
		log.Fatal("Data Path Error", err)
	}
	datapath = filepath.Join(path, "op_data")
	fmt.Println("Datapath: ", datapath)

	if _, err := os.Stat(datapath); os.IsNotExist(err) {
		err = os.Mkdir(datapath, 0755)
		if err != nil {
			log.Fatal("Data Folder creation Error", err)
		}
	}

	var dbpath string = filepath.Join(path, "data.db")

	_, err = os.Stat(dbpath)
	if err != nil {
		_, err = os.Create(dbpath)
		if err != nil {
			log.Fatal("DB Create Error", err)
		}
	}

	db, err := sql.Open("sqlite3", dbpath)
	if err != nil {
		log.Fatal("DB Open Error", err)
	}
	log.Println("Starting with DB: ", dbpath)
	DB = db

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS user
					(userid 	INTEGER PRIMARY KEY,
					email 		VARCHAR(255) NOT NULL,
					password 	BINARY(60) NOT NULL
					);`)
	if err != nil {
		log.Fatal("DB User Table Create Error", err)
	}
	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS session
					(sessionid 	INTEGER PRIMARY KEY,
					userid 		INTEGER NOT NULL,
					token 		CHAR(512) NOT NULL UNIQUE,
					expires 	BIGINT NOT NULL,
					FOREIGN KEY (userid) REFERENCES user(userid)
					);`)
	if err != nil {
		log.Fatal("DB session Table Create Error", err)
	}

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS grouped
					(groupid 	INTEGER PRIMARY KEY,
					name 		VARCHAR(255) NOT NULL
					);`)
	if err != nil {
		log.Fatal("DB grouped Table Create Error", err)
	}

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS user_group
					(ugid 		INTEGER PRIMARY KEY,
					uid 		INTEGER NOT NULL,
					gid 		INTEGER NOT NULL,
					FOREIGN KEY (uid) REFERENCES user(userid),
					FOREIGN KEY (gid) REFERENCES grouped(groupid)
	);`)
	if err != nil {
		log.Fatal("DB user_grouped Table Create Error", err)
	}

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS docs
					(docid 		INTEGER PRIMARY KEY,
					uowner 		INTEGER NULL,
					ugroup 		INTEGER NULL,
					name 		VARCHAR(256) NOT NULL,
					description VARCHAR(512) NULL,
					path 		VARCHAR(256) NOT NULL,
					FOREIGN KEY (uowner) REFERENCES user(userid),
					FOREIGN KEY (ugroup) REFERENCES grouped(groupid)
					);`)
	if err != nil {
		log.Fatal("DB docs Table Create Error", err)
	}

	fmt.Println("Server Protocol:", cfg.Section("server").Key("protocol").In("http", []string{"http", "https"}))
}

func main() {
	router := gin.Default()
	routing(router)
}

func getUser(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Comming soon"})
}

func routing(r *gin.Engine) {
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"POST, GET, OPTIONS, PUT, DELETE, UPDATE"},
		AllowHeaders:     []string{"Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	r.MaxMultipartMemory = 8 << 20 // 8 MiB
	r.POST("/upload", upload)

	r.StaticFS("/more_static", http.Dir("html"))
	r.GET("/user", getUser)
	r.POST("/user/register", createUser)
	r.POST("/user/login", loginUser)
	r.Run("localhost:" + fmt.Sprintf("%d", port))
}
