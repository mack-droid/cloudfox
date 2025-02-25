package aws

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/BishopFox/cloudfox/internal"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	ramTypes "github.com/aws/aws-sdk-go-v2/service/ram/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/bishopfox/awsservicemap"
	"github.com/sirupsen/logrus"
)

type RAMModule struct {
	// General configuration data
	RAMClient *ram.Client

	Caller       sts.GetCallerIdentityOutput
	AWSRegions   []string
	OutputFormat string
	Goroutines   int
	AWSProfile   string
	WrapTable    bool

	// Main module data
	Resources      []Resource
	CommandCounter internal.CommandCounter
	// Used to store output data for pretty printing
	output internal.OutputData2
	modLog *logrus.Entry
}

type Resource struct {
	AWSService string
	Region     string
	Name       string
	Owner      string
	Type       string
	ShareType  string
}

func (m *RAMModule) PrintRAM(outputFormat string, outputDirectory string, verbosity int) {
	// These stuct values are used by the output module
	m.output.Verbosity = verbosity
	m.output.Directory = outputDirectory
	m.output.CallingModule = "ram"
	m.modLog = internal.TxtLog.WithFields(logrus.Fields{
		"module": m.output.CallingModule,
	})
	if m.AWSProfile == "" {
		m.AWSProfile = internal.BuildAWSPath(m.Caller)
	}

	fmt.Printf("[%s][%s] Enumerating shared resources for account %s.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), aws.ToString(m.Caller.Account))

	wg := new(sync.WaitGroup)

	// Create a channel to signal the spinner aka task status goroutine to finish
	spinnerDone := make(chan bool)
	//fire up the the task status spinner/updated
	go internal.SpinUntil(m.output.CallingModule, &m.CommandCounter, spinnerDone, "regions")

	//create a channel to receive the objects
	dataReceiver := make(chan Resource)

	// Create a channel to signal to stop
	receiverDone := make(chan bool)

	go m.Receiver(dataReceiver, receiverDone)

	for _, region := range m.AWSRegions {
		wg.Add(1)
		m.CommandCounter.Pending++
		go m.executeChecks(region, wg, dataReceiver)

	}

	wg.Wait()
	//time.Sleep(time.Second * 2)

	// Send a message to the spinner goroutine to close the channel and stop
	spinnerDone <- true
	<-spinnerDone
	receiverDone <- true
	<-receiverDone

	// add - if struct is not empty do this. otherwise, dont write anything.
	m.output.Headers = []string{
		"Service",
		"Region",
		"Share Name",
		"Type",
		"Owner",
		"Share Type",
	}

	// Table rows
	for i := range m.Resources {
		m.output.Body = append(
			m.output.Body,
			[]string{
				m.Resources[i].AWSService,
				m.Resources[i].Region,
				m.Resources[i].Name,
				m.Resources[i].Type,
				m.Resources[i].Owner,
				m.Resources[i].ShareType,
			},
		)

	}
	if len(m.output.Body) > 0 {
		m.output.FilePath = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", aws.ToString(m.Caller.Account), m.AWSProfile))
		//m.output.OutputSelector(outputFormat)
		//utils.OutputSelector(verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule)
		//internal.OutputSelector(verbosity, outputFormat, m.output.Headers, m.output.Body, m.output.FilePath, m.output.CallingModule, m.output.CallingModule, m.WrapTable, m.AWSProfile)
		o := internal.OutputClient{
			Verbosity:     verbosity,
			CallingModule: m.output.CallingModule,
			Table: internal.TableClient{
				Wrap: m.WrapTable,
			},
		}
		o.Table.TableFiles = append(o.Table.TableFiles, internal.TableFile{
			Header: m.output.Headers,
			Body:   m.output.Body,
			Name:   m.output.CallingModule,
		})
		o.PrefixIdentifier = m.AWSProfile
		o.Table.DirectoryName = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", aws.ToString(m.Caller.Account), m.AWSProfile))
		o.WriteFullOutput(o.Table.TableFiles, nil)
		//m.writeLoot(o.Table.DirectoryName, verbosity)
		fmt.Printf("[%s][%s] %s resources found.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), strconv.Itoa(len(m.output.Body)))
	} else {
		fmt.Printf("[%s][%s] No resources found, skipping the creation of an output file.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile))
	}
	fmt.Printf("[%s][%s] For context and next steps: https://github.com/BishopFox/cloudfox/wiki/AWS-Commands#%s\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), m.output.CallingModule)
}

func (m *RAMModule) executeChecks(r string, wg *sync.WaitGroup, dataReceiver chan Resource) {
	defer wg.Done()
	servicemap := &awsservicemap.AwsServiceMap{
		JsonFileSource: "DOWNLOAD_FROM_AWS",
	}
	res, err := servicemap.IsServiceInRegion("ram", r)
	if err != nil {
		m.modLog.Error(err)
	}
	if res {
		m.CommandCounter.Total++
		m.CommandCounter.Pending--
		m.CommandCounter.Executing++
		m.getRAMResourcesPerRegion(r, dataReceiver)
		m.CommandCounter.Executing--
		m.CommandCounter.Complete++
	}
}

func (m *RAMModule) Receiver(receiver chan Resource, receiverDone chan bool) {
	defer close(receiverDone)
	for {
		select {
		case data := <-receiver:
			m.Resources = append(m.Resources, data)
		case <-receiverDone:
			receiverDone <- true
			return
		}
	}
}

func (m *RAMModule) getRAMResourcesPerRegion(r string, dataReceiver chan Resource) {
	// "PaginationMarker" is a control variable used for output continuity, as AWS return the output in pages.
	var PaginationControl *string
	var PaginationControl2 *string
	var shareName string
	var resourceType string
	shareTypes := []ramTypes.ResourceOwner{"SELF", "OTHER-ACCOUNTS"}

	for _, shareType := range shareTypes {
		for {
			GetResourceShares, err := m.RAMClient.GetResourceShares(
				context.TODO(),
				&ram.GetResourceSharesInput{
					NextToken:     PaginationControl,
					ResourceOwner: shareType,
				},
				func(o *ram.Options) {
					o.Region = r
				},
			)
			if err != nil {
				m.modLog.Error(err.Error())
				m.CommandCounter.Error++
				break
			}

			for _, resourceShare := range GetResourceShares.ResourceShares {
				var resourceShareArns []string
				shareName = aws.ToString(resourceShare.Name)
				resourceShareArns = append(resourceShareArns, aws.ToString(resourceShare.ResourceShareArn))
				ownerID := aws.ToString(resourceShare.OwningAccountId)

				for {
					ListResources, err := m.RAMClient.ListResources(
						context.TODO(),
						&ram.ListResourcesInput{
							NextToken:         PaginationControl2,
							ResourceOwner:     shareType,
							ResourceShareArns: resourceShareArns,
						},
						func(o *ram.Options) {
							o.Region = r
						},
					)
					if err != nil {
						m.modLog.Error(err.Error())
						m.CommandCounter.Error++
						break
					}

					for _, resource := range ListResources.Resources {
						resourceType = aws.ToString(resource.Type)
						var shareDirection string
						if string(shareType) == "OTHER-ACCOUNTS" {
							shareDirection = "Inbound share (Another account shared this with me)"
						} else {
							shareDirection = "Outbound share (I've shared this resource with others)"
						}

						dataReceiver <- Resource{
							AWSService: "RAM",
							Name:       shareName,
							Type:       resourceType,
							Region:     r,
							Owner:      ownerID,
							ShareType:  shareDirection,
						}
					}
					if ListResources.NextToken != nil {
						PaginationControl2 = ListResources.NextToken
					} else {
						PaginationControl2 = nil
						break
					}

				}
			}

			// The "NextToken" value is nil when there's no more data to return.
			if GetResourceShares.NextToken != nil {
				PaginationControl = GetResourceShares.NextToken
			} else {
				PaginationControl = nil
				break
			}
		}
	}
}
