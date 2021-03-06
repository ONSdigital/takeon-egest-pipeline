package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/sqs"
)

var region = os.Getenv("AWS_REGION")
var bucket = os.Getenv("S3_BUCKET")

// SurveyPeriods arrays in JSON message
type SurveyPeriods struct {
	Survey string `json:"survey"`
	Period string `json:"period"`
}

// InputJSON contains snapshot_id and array of surveyperiod combinations
type InputJSON struct {
	SnapshotID    string          `json:"snapshot_id"`
	SurveyPeriods []SurveyPeriods `json:"surveyperiods"`
}

// OutputMessage to send to export-output queue
type OutputMessage struct {
	SnapshotID string `json:"snapshot_id"`
	Location   string `json:"location"`
	Successful bool   `json:"successful"`
}

func main() {

	lambda.Start(handle)

}

func handle(ctx context.Context, sqsEvent events.SQSEvent) error {
	fmt.Println("Starting the application...")

	if len(sqsEvent.Records) == 0 {
		return errors.New("No SQS message passed to function")
	}

	for _, message := range sqsEvent.Records {
		fmt.Printf("The message %s for event source %s = %s \n", message.MessageId, message.EventSource, message.Body)
		queueMessage := message.Body
		messageDetails := []byte(queueMessage)
		var messageJSON InputJSON
		parseError := json.Unmarshal(messageDetails, &messageJSON)
		if parseError != nil {
			sendToSqs("", "null", false)
			return errors.New("Error with JSON from input queue" + parseError.Error())
		}
		inputMessage, validateError := validateInputMessage(messageJSON)
		if validateError != nil {
			return errors.New("Error with message from input queue")
		}
		snapshotID := inputMessage.SnapshotID
		var filename, err = getFileName(snapshotID, messageJSON.SurveyPeriods)
		if err != nil {
			return errors.New("Unable to create filename. Invalid Survey Period")
		}
		fmt.Println("File Name: ", filename)
		data, dataError := callGraphqlEndpoint(queueMessage, snapshotID, filename)
		if dataError != nil {
			sendToSqs(snapshotID, "null", false)
			return errors.New("Problem with call to Business Layer")
		}
		saveToS3(data, filename)
	}
	return nil
}

func getFileName(snapshotID string, surveyPeriods []SurveyPeriods) (string, error) {
	var combinedSurveyPeriods = ""
	var join = ""
	var filename = ""

	if len(surveyPeriods) == 0 {
		return filename, errors.New("Survey Period Invalid")
	}
	for _, item := range surveyPeriods {
		combinedSurveyPeriods = combinedSurveyPeriods + join + item.Survey + "_" + item.Period
		join = "-"
	}
	var bucketFilenamePrefix = "snapshot"
	filename = strings.Join([]string{bucketFilenamePrefix, combinedSurveyPeriods, snapshotID}, "-")
	return filename, nil
}

func callGraphqlEndpoint(message string, snapshotID string, filename string) (string, error) {
	var gqlEndpoint = os.Getenv("GRAPHQL_ENDPOINT")
	fmt.Println("Going to access  Graphql Endpoint: ", gqlEndpoint)
	response, err := http.Post(gqlEndpoint, "application/json; charset=UTF-8", strings.NewReader(message))
	fmt.Println("Message sending over to BL: ", message)
	if err != nil {
		fmt.Printf("The HTTP request failed with error %s\n", err)
		sendToSqs(snapshotID, "null", false)
	} else {
		data, _ := ioutil.ReadAll(response.Body)
		cdbExport := string(data)
		fmt.Println("Data from BL after successful call: " + cdbExport)
		if strings.Contains(cdbExport, "Error loading data for db Export") {
			return "", errors.New("Error with Business Layer")
		}
		fmt.Println("Accessing Graphql Endpoint done")
		sendToSqs(snapshotID, filename, true)
		return cdbExport, nil
	}
	return "", nil
}

func saveToS3(dbExport string, filename string) {

	fmt.Printf("Region: %q\n", region)
	config := &aws.Config{
		Region: aws.String(region),
	}

	sess := session.New(config)

	uploader := s3manager.NewUploader(sess)

	fmt.Printf("Bucket filename: %q\n", filename)

	reader := strings.NewReader(string(dbExport))
	var err error
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(filename),
		Body:   reader,
	})

	if err != nil {
		fmt.Printf("Unable to upload %q to %q, %v", filename, bucket, err)
	}

	fmt.Printf("Successfully uploaded %q to s3 bucket %q\n", filename, bucket)

}

func sendToSqs(snapshotid string, filename string, successful bool) {

	queue := aws.String(os.Getenv("DB_EXPORT_OUTPUT_QUEUE"))

	fmt.Printf("Region: %q\n", region)
	config := &aws.Config{
		Region: aws.String(region),
	}

	sess := session.New(config)

	svc := sqs.New(sess)

	urlResult, err := svc.GetQueueUrl(&sqs.GetQueueUrlInput{
		QueueName: queue,
	})

	if err != nil {
		fmt.Printf("Unable to find DB Export input queue %q, %q", *queue, err)
	}

	location := "s3://" + bucket + "/" + filename

	queueURL := urlResult.QueueUrl

	outputMessage := &OutputMessage{
		SnapshotID: snapshotid,
		Location:   location,
		Successful: successful,
	}

	DataToSend, err := json.Marshal(outputMessage)
	if err != nil {
		fmt.Printf("An error occured while marshaling DataToSend: %s", err)
	}
	fmt.Printf("DataToSend %v\n", string(DataToSend))

	_, error := svc.SendMessage(&sqs.SendMessageInput{
		MessageBody: aws.String(string(DataToSend)),
		QueueUrl:    queueURL,
	})

	if error != nil {
		fmt.Printf("Unable to send to DB Export output queue %q, %q", *queue, error)
	}

}

func validateInputMessage(messageJSON InputJSON) (InputJSON, error) {
	if messageJSON.SnapshotID == "" {
		sendToSqs("", "null", false)
		return messageJSON, errors.New("No SnapshotID given in message")
	} else if len(messageJSON.SurveyPeriods) == 0 {
		sendToSqs(messageJSON.SnapshotID, "null", false)
		return messageJSON, errors.New("No Survey/period combinations given in message")
	}
	return messageJSON, nil
}
