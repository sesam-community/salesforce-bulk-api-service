package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

//Template for Salesforce Bulk API URL
const BulkServiceUrl = `https://{instance}.salesforce.com/services/async/{api_version}/job`
const RestApiServiceUrl = `https://{instance}.salesforce.com/services/data/v{api_version}/`

//Salesforce Bulk API operation types
type Operation string

//Operations on Salesforce Bulk API
const (
	Insert   Operation = "insert"
	Update   Operation = "update"
	Delete   Operation = "delete"
	Upsert   Operation = "upsert"
	Query    Operation = "query"
	QueryAll Operation = "queryAll"
)

//Batch state
type State string

const (
	Open      State = "Open"
	Queued    State = "Queued"
	Closed    State = "Closed"
	Completed State = "Completed"
	Failed    State = "Failed"
	Error     State = "Error"
)

var KnownCompoundFields = [...]string{}
var AllowedTypes = []string{"id", "reference", "datetime"}

//Struct representing Job request
//https://developer.salesforce.com/docs/atlas.en-us.api_asynch.meta/api_asynch/asynch_api_quickstart_create_job.htm
type JobRequest struct {
	Operation   Operation `json:"operation"`
	Object      string    `json:"object"`
	ContentType string    `json:"contentType"`
}

//Struct representing Job response
//https://developer.salesforce.com/docs/atlas.en-us.api_asynch.meta/api_asynch/asynch_api_quickstart_create_job.htm
type Job struct {
	XMLName                 xml.Name
	Id                      string    `json:"id"`
	Operation               Operation `json:"operation"`
	Object                  string    `json:"object"`
	CreatedById             string    `json:"createdById"`
	CreatedDate             string    `json:"createdDate"`
	State                   State     `json:"state"`
	ConcurrencyMode         string    `json:"concurrencyMode"`
	ContentType             string    `json:"contentType"`
	NumberBatchesQueued     int       `json:"numberBatchesQueued"`
	NumberBatchesInProgress int       `json:"numberBatchesInProgress"`
	NumberBatchesCompleted  int       `json:"numberBatchesCompleted"`
	NumberBatchesFailed     int       `json:"numberBatchesFailed"`
	NumberBatchesTotal      int       `json:"numberBatchesTotal"`
	NumberRecordsProcessed  int       `json:"numberRecordsProcessed"`
	NumberRetries           int       `json:"numberRetries"`
	ApiVersion              float32   `json:"apiVersion"`
	NumberRecordsFailed     int       `json:"numberRecordsFailed"`
	TotalProcessingTime     int       `json:"totalProcessingTime"`
	ApiActiveProcessingTime int       `json:"apiActiveProcessingTime"`
	ApexProcessingTimeint   int       `json:"apexProcessingTime"`
	Batch                   []batch
	ObjectFields            []string
}

//Struct representing root element for batch response
//https://developer.salesforce.com/docs/atlas.en-us.api_asynch.meta/api_asynch/asynch_api_quickstart_add_batch.htm
type batch struct {
	Id                      string `json:"id"`
	JobId                   string `json:"jobId"`
	State                   State  `json:"state"`
	StateMessage            string `json:"stateMessage"`
	CreatedDate             string `json:"createdDate"`
	SystemModstamp          string `json:"systemModstamp"`
	NumberRecordsProcessed  int    `json:"numberRecordsProcessed"`
	NumberRecordsFailed     int    `json:"numberRecordsFailed"`
	TotalProcessingTime     int    `json:"totalProcessingTime"`
	ApiActiveProcessingTime int    `json:"apiActiveProcessingTime"`
	ApexProcessingTime      int    `json:"apexProcessingTime"`
}

//Salesforce API
type Api struct {
	apiVersion string
	instance   string
	username   string
	userId     string
	password   string
	sessionId  string
	serverUrl  string
	client     *http.Client
}

func CreateNew(username string, password string) *Api {
	tr := &http.Transport{
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	httpClient := &http.Client{Transport: tr}
	return &Api{username: username, password: password, apiVersion: "44.0", client: httpClient}
}

//LoginSoap authenticates using credentials provided  in Api object
func (api *Api) LoginSoap(sandbox bool) error {
	var env = "test"
	if !sandbox {
		env = "login"
	}
	var loginUrl = formatString(LOGIN_URL, "{env}", env, "{api_version}", api.apiVersion)
	var reqPayload = formatString(LOGIN_XML_PAYLOAD, "{username}", api.username, "{password}", api.password)

	req, err := http.NewRequest("POST", loginUrl, strings.NewReader(reqPayload))

	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "text/xml; charset=UTF-8")
	req.Header.Add("SOAPAction", "login")
	resp, err := api.client.Do(req)

	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		//Salesforce sends 500 HTTP and SOAP body with explanation
		faultResponse := &SalesforceLoginSoapResponse{}
		bodyBytes, innerErr := ioutil.ReadAll(resp.Body)
		if innerErr != nil {
			return innerErr
		}
		innerErr = xml.Unmarshal(bodyBytes, faultResponse)
		if innerErr != nil {
			return innerErr
		}
		return errors.New(fmt.Sprintf("Error: %s caused %s",
			faultResponse.Body.Fault.FaultCode, faultResponse.Body.Fault.FaultString))
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	successRes := &SalesforceLoginSoapResponse{}

	err = xml.Unmarshal(bodyBytes, successRes)
	if err != nil {
		return err
	}

	api.serverUrl = successRes.Body.Success.Result.ServerUrl
	api.sessionId = successRes.Body.Success.Result.SessionId
	api.userId = successRes.Body.Success.Result.UserId
	api.instance = extractInstanceFromUrl(api.serverUrl)

	return nil
}

//Add fields available for queering in Bulk API to Job object
func (job *Job) populateObjectFields(objectFields []interface{}) {
	for _, v := range objectFields {
		field := v.(map[string]interface{})
		fieldName := field["name"]
		if true == field["createable"] || stringInArray(field["type"].(string), AllowedTypes) {
			job.ObjectFields = append(job.ObjectFields, fieldName.(string))
		}
	}
}

//
//DescribeSObject returns description for Salesforce object with given name
//
func (api *Api) DescribeSObject(name string) (result map[string]interface{}, err error) {
	var reqUrl = fmt.Sprintf("%ssobjects/%s/describe",
		formatString(RestApiServiceUrl, "{instance}", api.instance, "{api_version}", api.apiVersion), name)

	req, err := http.NewRequest("GET", reqUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", api.sessionId))

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("HTTP %d - %s", resp.StatusCode, resp.Status))
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

//
// CreateJob - create new Job
//
func (api *Api) CreateJob(operation Operation, sObject string, contentType string) (*Job, error) {
	var result = &Job{}
	var jobRequestPayload = &JobRequest{}
	var byteBuf = new(bytes.Buffer)

	jobRequestPayload.Object = sObject
	jobRequestPayload.ContentType = contentType
	jobRequestPayload.Operation = operation

	err := json.NewEncoder(byteBuf).Encode(jobRequestPayload)
	if err != nil {
		return result, err
	}

	var serviceUrl = formatString(BulkServiceUrl, "{instance}", api.instance, "{api_version}", api.apiVersion)

	req, err := http.NewRequest("POST", serviceUrl, byteBuf)
	if err != nil {
		return result, err
	}

	req.Header.Add("X-SFDC-Session", api.sessionId)
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	resp, err := api.client.Do(req)

	if err != nil {
		return result, err
	}

	defer resp.Body.Close()

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}

	err = json.Unmarshal(bodyBytes, result)
	if err != nil {
		return result, err
	}

	return result, nil
}

//
//AddBatchToJob - add new batch to previously created Salesforce batch job
//
func (api *Api) AddBatchToJob(job *Job, since time.Time) error {
	var reqPayload string
	var jobUrl = formatString(BulkServiceUrl,
		"{instance}", api.instance, "{api_version}", api.apiVersion) + "/" + job.Id + "/batch"
	var batch = &batch{}

	switch job.Operation {
	case Query:
		if len(job.ObjectFields) == 0 {
			return errors.New("batch query must have at least one field")
		}
		reqPayload = fmt.Sprintf("SELECT %s from %s", strings.Join(job.ObjectFields, ", "), job.Object)
		if !since.IsZero() {
			reqPayload += fmt.Sprintf(" WHERE LastModifiedDate >= %s", since.Format("2006-01-02T15:04:05-0700"))
		}
		break
	default:
		return errors.New("unrecognized job operation. We support only query operation right now")
	}

	req, err := http.NewRequest("POST", jobUrl, strings.NewReader(reqPayload))
	if err != nil {
		return err
	}

	req.Header.Add("X-SFDC-Session", api.sessionId)
	req.Header.Add("Content-Type", "application/json") //fixme define type dynamically
	resp, err := api.client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(bodyBytes, batch)
	if err != nil {
		fmt.Println(string(bodyBytes[:]))
		return err
	}
	job.Batch = append(job.Batch, *batch)

	return nil
}

//CheckJobStatus check and return status of current job
//status returned is Completed or first "non completed" batch status
func (api *Api) CheckJobStatus(job *Job) (State, error) {

	for idx := range job.Batch {
		jobUrl := formatString(BulkServiceUrl,
			"{instance}", api.instance, "{api_version}", api.apiVersion) + "/" + job.Id + "/batch/" + job.Batch[idx].Id
		req, err := http.NewRequest("GET", jobUrl, nil)
		if err != nil {
			return Error, err
		}
		req.Header.Add("X-SFDC-Session", api.sessionId)
		req.Header.Add("Accept", "application/json")
		resp, err := api.client.Do(req)
		if err != nil {
			return Error, err
		}

		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			return Error, err
		}

		err = json.Unmarshal(bodyBytes, &job.Batch[idx])
		if err != nil {
			return Error, err
		}
		if job.Batch[idx].State != Completed {
			return job.Batch[idx].State, nil
		}
	}
	return Completed, nil
}

func (api *Api) GetJobResult(job *Job) ([]interface{}, error) {
	var resultList []interface{}

	for idx := range job.Batch {
		var resultIds []string
		url := formatString(BulkServiceUrl,
			"{instance}", api.instance, "{api_version}", api.apiVersion) +
			"/" + job.Id + "/batch/" + job.Batch[idx].Id + "/result"

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return resultList, err
		}
		req.Header.Add("X-SFDC-Session", api.sessionId)
		req.Header.Add("Accept", "application/json")
		resp, err := api.client.Do(req)
		if err != nil {
			return resultList, err
		}

		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			return resultList, err
		}
		err = json.Unmarshal(bodyBytes, &resultIds)
		if err != nil {
			return resultList, err
		}

		for resultIdx := range resultIds {
			var localResultList []interface{}

			resultUrl := formatString(BulkServiceUrl,
				"{instance}", api.instance, "{api_version}", api.apiVersion) +
				"/" + job.Id + "/batch/" + job.Batch[idx].Id + "/result/" + resultIds[resultIdx]
			req, err := http.NewRequest("GET", resultUrl, nil)
			if err != nil {
				return resultList, err
			}
			req.Header.Add("X-SFDC-Session", api.sessionId)
			req.Header.Add("Accept", "application/json")
			resp, err := api.client.Do(req)
			if err != nil {
				return resultList, err
			}

			bodyBytes, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return resultList, err
			}
			//fmt.Println(string(bodyBytes[:]))

			err = json.Unmarshal(bodyBytes, &localResultList)
			if err != nil {
				return resultList, err
			}
			resultList = append(resultList, localResultList...)
		}
	}
	return resultList, nil
}

//CloseJob - close Bulk API job and free resources allocated at Salesforce instance
func (api *Api) CloseJob(job *Job) error {
	jobUrl := formatString(BulkServiceUrl,
		"{instance}", api.instance, "{api_version}", api.apiVersion) + "/" + job.Id
	req, err := http.NewRequest("POST", jobUrl, strings.NewReader(`{"state": "Closed"}`))
	if err != nil {
		return err
	}
	req.Header.Add("X-SFDC-Session", api.sessionId)
	req.Header.Add("Content-Type", "application/json")
	resp, err := api.client.Do(req)
	if err != nil {
		return err
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()

	if err != nil {
		return err
	}

	err = json.Unmarshal(bodyBytes, job)
	if err != nil {
		return err
	}

	if job.State != Closed {
		return errors.New(fmt.Sprintf("Something went wrong. Job state is %s", job.State))
	}
	return nil
}
