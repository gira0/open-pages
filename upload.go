package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/rs/xid"
)

type DocCreate struct {
	Name        string `json:"name" binding:"required,alphanum"`
	Description string `json:"description" binding:"alphanumunicode"`
}

func docCreate(c *gin.Context) {
	newDoc := DocCreate{}
	if err := c.ShouldBindJSON(&newDoc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userid := c.MustGet("userid")

	_, err := DB.Exec("INSERT INTO docs (uowner, name, description) VALUES (?,?,?);", userid, newDoc.Name, newDoc.Description)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error2": err.Error()})
		return // DB Error while inserting
	}
}

func docUpload(c *gin.Context) {
	// Check header
	guid := xid.New()
	path := filepath.Join(datapath, guid.String())

	switch header := c.Request.Header["Content-Type"][0]; header {
	case "application/x-tar":
		fmt.Println(header)
	case "application/gzip":
		fmt.Println(header)
		gzipdata, err := c.GetRawData()
		if err != nil {
			fmt.Println("error", err)
		}
		ungztar(gzipdata, path)

	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Wront Content-Type. Recieved: " + header,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "File uploaded",
	})
}

// unzip() {

func ungztar(data []byte, dest string) error {
	// Convert the byte array to an io.Reader
	r := bytes.NewReader(data)

	// Create a new gzip reader
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	// Create a new tar reader
	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()

		switch {

		// if no more files are found return
		case err == io.EOF:
			return nil

		// return any other error
		case err != nil:
			return err

		// if the header is nil, just skip it (not sure how this happens)
		case header == nil:
			continue
		}

		// the target location where the dir/file should be created
		target := filepath.Join(dest, header.Name)

		// the following switch could also be done using fi.Mode(), not sure if there
		// a benefit of using one vs. the other.
		// fi := header.FileInfo()

		// check the file type
		switch header.Typeflag {

		// if its a dir and it doesn't exist create it
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}

		// if it's a file create it
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			// copy over contents
			if _, err := io.Copy(f, tr); err != nil {
				return err
			}

			// manually close here after each file operation; defering would cause each file close
			// to wait until all operations have completed.
			f.Close()
		}
	}
}
