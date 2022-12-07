package main

import (
	"database/sql"
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

var DB *sql.DB

func main() {
	cfg, err := ini.Load("settings.ini")
	if err != nil {
		fmt.Printf("Fail to read file: %v", err)
		os.Exit(1)
	}

	var port int = cfg.Section("server").Key("port").MustInt(8080)
	var datapath string = cfg.Section("paths").Key("datapath").MustString("./")
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

	_, err = DB.Exec("CREATE TABLE IF NOT EXISTS user (userid INTEGER PRIMARY KEY,email varchar(255),password BINARY(60));")
	if err != nil {
		log.Fatal("DB Table Create Error", err)
	}

	fmt.Println("Server Protocol:", cfg.Section("server").Key("protocol").In("http", []string{"http", "https"}))

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

	router.POST("/user", createUser)
	router.POST("/user/login", loginUser)
	router.Run("localhost:" + fmt.Sprintf("%d", port))
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
			c.JSON(http.StatusConflict, gin.H{"error": "User already exists"})
			return
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error4": err.Error()})
			return
		}
	}
}

func loginUser(c *gin.Context) {
	login := Auth{}

	if err := c.ShouldBindJSON(&login); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var result Auth
	if err := DB.QueryRow("SELECT email,password FROM user WHERE email=?", login.Email).Scan(&result.Email, &result.Password); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error1": "Unauthorized"})
			return

		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
			return
		}

	} else {
		if login.Email == result.Email {
			err = bcrypt.CompareHashAndPassword([]byte(result.Password), []byte(login.Password))
			if err != nil {
				if err == bcrypt.ErrMismatchedHashAndPassword {
					c.JSON(http.StatusUnauthorized, gin.H{"error3": "Unauthorized"})
					return
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error4": err.Error()})
					return
				}

			} else {
				c.JSON(http.StatusOK, gin.H{"status": "Successful login"})
				return
			}

		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"error5": "Unauthorized"})
			return
		}
	}
}
