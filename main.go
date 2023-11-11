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
var tmppath string

func init() {
	// Init for ini settings
	cfg, err := ini.Load("settings.ini")
	if err != nil {
		fmt.Printf("Fail to read file: %v", err)
		os.Exit(1)
	}
	// Init readout
	port = cfg.Section("server").Key("port").MustInt(8080)
	datapath = cfg.Section("paths").Key("datapath").MustString("./")
	tmppath = cfg.Section("paths").Key("tmppath").MustString("./")
	cookie_secure = cfg.Section("sever").Key("cookie_secure").MustBool(false)
	cookie_httpOnly = cfg.Section("server").Key("cookie_httpOnly").MustBool(true)

	// Setup data path for folders
	dpath, err := filepath.Abs(datapath)
	if err != nil {
		log.Fatal("Data Path Error: ", err)
	}
	datapath = filepath.Join(dpath, "op_data")
	fmt.Println("Datapath: ", datapath)

	// Setup tmp path for folders
	tpath, err := filepath.Abs(tmppath)
	if err != nil {
		log.Fatal("Tmp Path Error: ", err)
	}
	tmppath = filepath.Join(tpath, "tmp")
	fmt.Println("Tmp Path: ", tmppath)

	if _, err := os.Stat(datapath); os.IsNotExist(err) {
		err = os.Mkdir(datapath, 0755)
		if err != nil {
			log.Fatal("Data Folder creation Error: ", err)
		}
	}

	if _, err := os.Stat(tmppath); os.IsNotExist(err) {
		err = os.Mkdir(tmppath, 0755)
		if err != nil {
			log.Fatal("Tmp Folder creation Error: ", err)
		}
	}

	var dbpath string = filepath.Join(dpath, "data.db")

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

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS groups
					(groupid 	INTEGER PRIMARY KEY,
					name 		VARCHAR(255) NOT NULL
					);`)
	if err != nil {
		log.Fatal("DB groups Table Create Error", err)
	}

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS user_group
					(ugid 		INTEGER PRIMARY KEY,
					uid 		INTEGER NOT NULL,
					gid 		INTEGER NOT NULL,
					FOREIGN KEY (uid) REFERENCES user(userid),
					FOREIGN KEY (gid) REFERENCES groups(groupid)
	);`)
	if err != nil {
		log.Fatal("DB user_groups Table Create Error", err)
	}

	_, err = DB.Exec(`CREATE TABLE IF NOT EXISTS docs
					(docid 		INTEGER PRIMARY KEY,
					uowner 		INTEGER NULL,
					ugroup 		INTEGER NULL,
					name 		VARCHAR(256) NOT NULL UNIQUE,
					description VARCHAR(512) NULL,
					path 		VARCHAR(256) NULL UNIQUE,
					FOREIGN KEY (uowner) REFERENCES user(userid),
					FOREIGN KEY (ugroup) REFERENCES groups(groupid)
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

func routing(r *gin.Engine) {
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"POST, GET, OPTIONS, PUT, DELETE, UPDATE"},
		AllowHeaders:     []string{"Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	r.LoadHTMLGlob("templates/**")
	r.GET("/index", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Posts",
		})
	})

	r.MaxMultipartMemory = 8 << 20 // 8 MiB
	v1 := r.Group("/v1")
	{
		v1.GET("/ping", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"message": "pong",
			})
		})

		v1.StaticFS("/more_static", http.Dir("html"))
		v1.POST("/user/register", createUser)
		v1.POST("/user/login", loginUser)

		auth := v1.Group("/auth")
		{
			auth.Use(AuthMiddleware)
			auth.GET("/user", getUser)
			auth.POST("/docs/create", docCreate)
			auth.POST("/docs/upload", rawDocUpload)
			auth.POST("/docs/formupload", formDocUpload)
		}

	}

	r.Run("localhost:" + fmt.Sprintf("%d", port))
}
