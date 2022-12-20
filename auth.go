package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

type Auth struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

var cookie_secure bool
var cookie_httpOnly bool

func AuthMiddleware(c *gin.Context) {
	// Check if auth cookie already set and valid
	if cookie, err := c.Cookie("auth_cookie"); err == nil {

		var userid int
		err := DB.QueryRow("SELECT userid FROM session WHERE token=$1 AND expires > $2", cookie, time.Now().Unix()).Scan(&userid)
		if err == nil {
			c.Set("userid", userid)
		} else if err == sql.ErrNoRows {
			// Delete cookie, it's invalid
			c.SetCookie("auth_cookie", "", -1, "/", "localhost", cookie_secure, cookie_httpOnly)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthenticated"})
			return
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"cookie_error": err})
			return
		}
	}

	c.Next()
}

func GenerateSecureToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
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
			c.SetCookie("auth_cookie", "", -1, "/", "localhost", cookie_secure, cookie_httpOnly)
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
			c.SetCookie("auth_cookie", sessionToken, 3600, "/", "localhost", cookie_secure, cookie_httpOnly)
			_, err = DB.Exec("INSERT INTO session (userid, token, expires) VALUES (?,?,?);", result.UserID, sessionToken, time.Now().Add(time.Hour*24*7).Unix())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
				return // DB Error while inserting
			}
			c.JSON(http.StatusOK, gin.H{"status": "successful login"})
			return // Login successful
		}
	}
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

func getUser(c *gin.Context) {
	userid := c.MustGet("userid")
	c.JSON(http.StatusOK, gin.H{"userid": userid})
}
