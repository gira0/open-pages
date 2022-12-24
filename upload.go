package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gabriel-vasile/mimetype"
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

func rawDocUpload(c *gin.Context) {
	// Check header
	guid := xid.New()
	path := filepath.Join(datapath, guid.String())

	data, err := c.GetRawData()
	if err != nil {
		fmt.Println("error", err)
	}
	mtype := mimetype.Detect(data)
	fmt.Println("MIME:", mtype)

	switch header := strings.Split(c.Request.Header["Content-Type"][0], ";")[0]; header {
	case "application/x-tar":

	case "application/gzip":
		ungztar(data, path)
	case "application/zip":
		unzip(data, path)
	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Wront Content-Type. Recieved: " + header,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "File uploaded",
	})
}

type DucUpload struct {
	File *multipart.File `form:"file" binding:"required"`
	Name string          `form:"name" binding:"required"`
}

func formDocUpload(c *gin.Context) {
	var form DucUpload
	if err := c.Bind(&form); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	guid := xid.New()
	path := filepath.Join(datapath, guid.String())

	formfile, _, err := c.Request.FormFile("file")
	if err != nil {
		fmt.Println("error", err)
	}
	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, formfile)
	if err != nil {
		fmt.Println("error", err)
	}
	mtype := mimetype.Detect(buf.Bytes())
	fmt.Println("MIME:", mtype)
	unzip(buf.Bytes(), path)
}

func unzip(data []byte, dest string) error {

	z := bytes.NewReader(data)

	// Create a new gzip reader
	buff := bytes.NewBuffer([]byte{})
	size, err := io.Copy(buff, z)
	if err != nil {
		return err
	}
	reader := bytes.NewReader(buff.Bytes())

	// Open a zip archive for reading.
	zipReader, err := zip.NewReader(reader, size)
	if err != nil {
		return err
	}

	for _, f := range zipReader.File {
		filePath := filepath.Join(dest, f.Name)
		//fmt.Println("unzipping file ", filePath)

		if !strings.HasPrefix(filePath, filepath.Clean(dest)+string(os.PathSeparator)) {
			//fmt.Println("invalid file path")
			return err
		}
		if f.FileInfo().IsDir() {
			//fmt.Println("creating directory...")
			os.MkdirAll(filePath, os.ModePerm)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
			return err
		}

		dstFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		fileInArchive, err := f.Open()
		if err != nil {
			return err
		}

		if _, err := io.Copy(dstFile, fileInArchive); err != nil {
			return err
		}

		dstFile.Close()
		fileInArchive.Close()
	}
	return nil
}

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
