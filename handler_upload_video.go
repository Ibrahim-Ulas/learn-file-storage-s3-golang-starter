package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	uploadLimit := http.MaxBytesReader(w, r.Body, 1<<30)
	r.Body = uploadLimit
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Printf("Uploading video %v by user %v", videoID, userID)
	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error fetching video from database", err)
		return
	}

	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video is someone else's bitch", err)
		return
	}
	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting file data", err)
		return
	}
	defer file.Close()
	mimeType := fileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error parsing mime type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Wrong mime type", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely_upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error resetting tempfile", err)
		return
	}
	processedFile, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	openedProcessedFile, err := os.Open(processedFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer openedProcessedFile.Close()
	defer os.Remove(processedFile)

	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error creating file name", err)
		return
	}

	randomPath := base64.RawURLEncoding.EncodeToString(randomBytes)
	filePrefix := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error getting temp file name", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		filePrefix = "landscape/"
	case "9:16":
		filePrefix = "portrait/"
	default:
		filePrefix = "other/"
	}

	fileKey := filePrefix + randomPath + ".mp4"
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        openedProcessedFile,
		ContentType: &mimeType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading object to bucket", err)
		return
	}

	videoURL := "https://" + cfg.s3Bucket + ".s3." + cfg.s3Region + ".amazonaws.com/" + fileKey
	videoMetadata.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}
	respondWithJSON(w, http.StatusOK, videoMetadata)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := bytes.Buffer{}
	cmd.Stdout = &buffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	type ratio struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	type streams struct {
		Streams []ratio `json:"streams"`
	}
	Ratio := streams{}
	err = json.Unmarshal(buffer.Bytes(), &Ratio)
	if err != nil {
		return "", err
	}
	width := Ratio.Streams[0].Width
	height := Ratio.Streams[0].Height
	realRatio := ""
	if width > height {
		if width/16 == height/9 {
			realRatio = "16:9"
		} else {
			realRatio = "other"
		}

	} else {
		if width/9 == height/16 {
			realRatio = "9:16"
		} else {
			realRatio = "other"
		}
	}
	return realRatio, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"

	command := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := command.Run()
	if err != nil {
		return "", err
	}
	return outputFilePath, nil
}
