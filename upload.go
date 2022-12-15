package main

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

func upload(c *gin.Context) {
	// single file
	file, err := c.FormFile("file")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"message": "No file is received",
		})
		return
	}

	fmt.Println(file.Filename)

	// Upload the file to specific dst.
	c.SaveUploadedFile(file, filepath.Join(datapath, "upload"))

	c.String(http.StatusOK, fmt.Sprintf("'%s' uploaded!", file.Filename))
}
