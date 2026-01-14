package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
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

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Video not found", err)
		return
	}

	if userID != dbVideo.UserID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}

	fileData, fileHeaders, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "form error", err)
		return
	}
	defer fileData.Close()

	contentType := fileHeaders.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid content type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "only video/mp4 is allowed", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "file system error", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, fileData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "file system error", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error creating file", err)
		return
	}

	randbits := make([]byte, 32)
	_, err = rand.Read(randbits)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error creating file", err)
		return
	}

	key := hex.EncodeToString(randbits) + ".mp4"

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error getting aspect ratio", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		key = "16:9/" + key
	case "9:16":
		key = "9:16/" + key
	default:
		key = "other/" + key
	}

	fastFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "trouble processing video", err)
		return
	}
	fastStartFile, err := os.Open(fastFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "trouble processing video", err)
		return
	}

	input := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        fastStartFile,
		ContentType: aws.String("video/mp4"),
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &input)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "s3 upload error", err)
		return
	}
	videoURL := "https://" + cfg.s3Bucket + ".s3." + cfg.s3Region + ".amazonaws.com/" + key
	dbVideo.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "db error", err)
		return
	}
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := &bytes.Buffer{}
	cmd.Stdout = buffer
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error running ffprobe: %v", err)
	}
	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	err = json.Unmarshal(buffer.Bytes(), &result)
	if err != nil {
		return "", fmt.Errorf("error parsing JSON: %v", err)
	}

	if len(result.Streams) < 1 {
		return "", fmt.Errorf("found no streams")
	}

	aspectRatio := float64(result.Streams[0].Width) / float64(result.Streams[0].Height)
	if math.Abs(aspectRatio-(16.0/9.0)) < .01 {
		return "16:9", nil
	} else if math.Abs(aspectRatio-(9.0/16.0)) < .01 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outFP := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outFP)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error running ffmpeg: %v", err)
	}
	return outFP, nil
}
