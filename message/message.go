package message

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LinioIT/rabbitmq-worker/logfile"
	"github.com/streadway/amqp"
	"io/ioutil"
	"net/http"
	"time"
)

// HttpRequestMessage holds all info and status for a RabbitMQ message and its associated http request.
type HttpRequestMessage struct {
	// RabbitMQ message
	Delivery amqp.Delivery

	// Message Id is either set as a RabbitMQ message header, or
	// calculated as the md5 hash of the RabbitMQ message body
	MessageId string

	// Http request fields
	Url     string
	Headers map[string]string
	Body    string

	// Time when message was originally created (if timestamp plugin was installed)
	Timestamp int64

	// Time when message will expire
	// (if not provided, value is calculated from DefaultTTL config setting)
	Expiration int64

	// Retry history from RabbitMQ headers
	RetryCnt           int
	FirstRejectionTime int64

	// Http request status
	HttpStatusMsg string
	HttpRespBody  string
	HttpErr       error

	// Drop / Retry Indicator - Set after http request attempt
	Drop bool
}

func (msg *HttpRequestMessage) Parse(rmqDelivery amqp.Delivery, logFile *logfile.Logger) (err error) {
	type MessageFields struct {
		Url     string
		Headers []map[string]string
		Body    string
	}

	var fields MessageFields

	msg.Delivery = rmqDelivery

	/*** Parse fields in RabbitMQ message body ***/
	if err := json.Unmarshal(rmqDelivery.Body, &fields); err != nil {
		return err
	}

	// Url
	if len(fields.Url) == 0 {
		err = errors.New("Field 'url' is empty or missing")
		return err
	}
	msg.Url = fields.Url

	// Http headers
	msg.Headers = make(map[string]string)
	for _, m := range fields.Headers {
		for key, val := range m {
			msg.Headers[key] = val
		}
	}

	// Request body
	msg.Body = fields.Body

	/*** Extract fields from RabbitMQ message properties ***/
	// Message creation timestamp
	if !rmqDelivery.Timestamp.IsZero() {
		msg.Timestamp = rmqDelivery.Timestamp.Unix()
	}

	/*** Extract fields from RabbitMQ message headers ***/
	rmqHeaders := rmqDelivery.Headers
	if rmqHeaders != nil {

		// Message expiration
		expirationHdr, ok := rmqHeaders["expiration"]
		if ok {
			expiration, ok := expirationHdr.(int64)
			if !ok || expiration < time.Now().Unix() {
				logFile.Write("Header value 'expiration' is invalid, or the expiration time has already past. Default TTL will be used.")
			} else {
				msg.Expiration = expiration
			}
		}

		// Message ID
		messageIdHdr, ok := rmqHeaders["message_id"]
		if ok {
			messageId, ok := messageIdHdr.(string)
			if !ok || len(messageId) == 0 {
				logFile.Write("Header value 'message_id' is invalid or empty. The Message ID will be the md5 hash of the RabbitMQ message body.")
			} else {
				msg.MessageId = messageId
			}
		}
		// Message ID was not provided, set it as the md5 hash of the message body. Append the original timestamp, if available.
		if len(msg.MessageId) == 0 {
			msg.MessageId = fmt.Sprintf("%x", md5.Sum(rmqDelivery.Body))
			if msg.Timestamp > 0 {
				msg.MessageId += fmt.Sprintf("-%d", msg.Timestamp)
			}
		}

		// Get retry count and time of first rejection. Both values will be zero if this is the first attempt.
		msg.RetryCnt, msg.FirstRejectionTime = getRetryInfo(rmqHeaders)
	}

	logFile.WriteDebug("Message fields:", msg)
	logFile.WriteDebug("Retry:", msg.RetryCnt)
	logFile.WriteDebug("First Rejection Time:", msg.FirstRejectionTime)

	return nil
}

func getRetryInfo(rmqHeaders amqp.Table) (retryCnt int, firstRejectionTime int64) {
	retryCnt = 0
	firstRejectionTime = 0

	deathHistory, ok := rmqHeaders["x-death"]
	if ok {
		// The RabbitMQ "death" history is provided as an array of 2 maps.  One map has the history for the wait queue, the other for the main queue.
		// The "count" field will have the same value in each map and it represents the # of times this message was dead-lettered to each queue.
		// As an example, if the count is currently two, then there have been two previous attempts to send this message and the upcoming attempt will be the 2nd retry.
		queueDeathHistory := deathHistory.([]interface{})
		if len(queueDeathHistory) == 2 {
			mainQueueDeathHistory := queueDeathHistory[1].(amqp.Table)

			// Get retry count
			retries, retriesOk := mainQueueDeathHistory["count"]
			if retriesOk {
				retryCnt = int(retries.(int64))
			}

			// Get time of first rejection
			rejectTime, rejectTimeOk := mainQueueDeathHistory["time"]
			if rejectTimeOk {
				if rejectTime != nil {
					firstRejectionTime = rejectTime.(time.Time).Unix()
				}
			}
		}
	}

	return
}

func (msg HttpRequestMessage) HttpPost(ackCh chan HttpRequestMessage, timeout int) {
	req, err := http.NewRequest("POST", msg.Url, bytes.NewBufferString(msg.Body))
	if err != nil {
		msg.HttpErr = err
		msg.HttpStatusMsg = "Invalid http request: " + err.Error()
		msg.Drop = true
		ackCh <- msg
		return
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	for hkey, hval := range msg.Headers {
		req.Header.Set(hkey, hval)
	}

	resp, err := client.Do(req)

	if err != nil {
		msg.HttpErr = err
		msg.HttpStatusMsg = "Error on http POST: " + err.Error()
		ackCh <- msg
		return
	} else {
		htmlData, err := ioutil.ReadAll(resp.Body)

		// The response body is not currently used to evaluate success of the http request. Therefore, an error here is not fatal.
		// This will change if functionality is added to evaluate the response body.
		if err != nil {
			msg.HttpRespBody = "Error encountered when reading POST response body"
		} else {
			msg.HttpStatusMsg = resp.Status
			msg.HttpRespBody = string(htmlData)
			resp.Body.Close()
		}
	}

	if resp.StatusCode >= 400 && resp.StatusCode <= 499 {
		msg.HttpErr = errors.New("4XX status on http POST (no retry): " + resp.Status)
		msg.Drop = true
		ackCh <- msg
		return
	}

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		msg.Drop = true
		ackCh <- msg
		return
	}

	msg.HttpErr = errors.New("Error on http POST: " + resp.Status)
	ackCh <- msg
}
