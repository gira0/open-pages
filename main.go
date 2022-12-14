package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

type Auth struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

type UserEntry struct {
	UserID   int    `json:"userid" binding:"required,numeric"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

type LoginResponse struct {
	AuthToken    string `json:"authtoken" binding:"required,jwt"`
	RefreshToken string `json:"refreshtoken" binding:"required,jwt"`
}

func GenerateSecureToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
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
	var dbpath string = path.Join(datapath, "data.db")

	_, err = os.Stat(dbpath)
	if err != nil {
		_, err = os.Create(dbpath)
		if err != nil {
			log.Fatal("DB Create Error", err)
		}
	}

	fmt.Println("DB found")
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

	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"POST, GET, OPTIONS, PUT, DELETE, UPDATE"},
		AllowHeaders:     []string{"Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	router.StaticFS("/more_static", http.Dir("htmlls"))
	router.GET("/user", getUser)
	router.POST("/user/register", createUser)
	router.POST("/user/login", loginUser)
	router.Run("localhost:" + fmt.Sprintf("%d", port))
}

func getUser(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Comming soon"})
}

func createUser(c *gin.Context) {
	newUser := Auth{}

	if err := c.ShouldBindJSON(&newUser); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var result string
	if err := DB.QueryRow("SELECT email FROM user WHERE email=?", newUser.Email).Scan(&result); err != nil {
		if err == sql.ErrNoRows {
			hash, err := bcrypt.GenerateFromPassword([]byte(newUser.Password), bcrypt.DefaultCost)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error1": err.Error()})
				return
			}
			_, err = DB.Exec("INSERT INTO user (email, password) VALUES (?,?);", newUser.Email, string(hash))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error3": err.Error()})
			return
		}
	} else {
		if newUser.Email == result {
			c.JSON(http.StatusOK, gin.H{"error": "User already exists"})
			return
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error4": err})
			return
		}
	}
}

func loginUser(c *gin.Context) {
	login := Auth{}

	// Get the expected POST Data
	if err := c.ShouldBindJSON(&login); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if auth cookie already set and valid
	if cookie, err := c.Cookie("auth_cookie"); err == nil {
		// fmt.Println("Cookie value: ", cookie)
		fmt.Println(cookie)
		var user int
		err := DB.QueryRow("SELECT userid FROM session WHERE token=$1 AND expires > $2", cookie, time.Now().Unix()).Scan(&user)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"error": "Already logged in"})
			return
		} else if err == sql.ErrNoRows {
			// Delete cookie, it's invalid
			c.SetCookie("auth_cookie", "", -1, "/", "localhost", false, false)
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"cookie_error": err})
			return
		}

	}

	var result UserEntry
	if err := DB.QueryRow("SELECT userid,email,password FROM user WHERE email=?", login.Email).Scan(&result.UserID, &result.Email, &result.Password); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error1": "Unauthorized"})
			return // Wrong user, email not found
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
			return // Unknown Error, not the not found error
		}
	} else {
		err = bcrypt.CompareHashAndPassword([]byte(result.Password), []byte(login.Password))
		if err != nil {
			if err == bcrypt.ErrMismatchedHashAndPassword {
				c.JSON(http.StatusUnauthorized, gin.H{"error3": "Unauthorized"})
				return // Password wrong
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error4": err.Error()})
				return // Unknown Error, not the not wrong password error
			}

		} else {
			sessionToken := GenerateSecureToken(32)
			c.SetCookie("auth_cookie", sessionToken, 3600, "/", "localhost", false, false)
			_, err = DB.Exec("INSERT INTO session (userid, token, expires) VALUES (?,?,?);", result.UserID, sessionToken, time.Now().Add(time.Hour*24*7).Unix())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
				return // DB Error while inserting
			}
			c.JSON(http.StatusOK, gin.H{"sessionToken": "token set"})
			return // Login successful
		}

	}
}
