// Copyright (c) 2015-2021, NVIDIA CORPORATION.
// SPDX-License-Identifier: Apache-2.0

package imgrpkg

import (
	"bytes"
	"container/list"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/NVIDIA/sortedmap"

	"github.com/NVIDIA/proxyfs/bucketstats"
	"github.com/NVIDIA/proxyfs/conf"
	"github.com/NVIDIA/proxyfs/ilayout"
	"github.com/NVIDIA/proxyfs/retryrpc"
	"github.com/NVIDIA/proxyfs/utils"
)

type configStruct struct {
	PrivateIPAddr  string
	PublicIPAddr   string
	RetryRPCPort   uint16 // To be served only on PublicIPAddr  via TLS
	HTTPServerPort uint16 // To be served only on PrivateIPAddr via TCP

	RetryRPCTTLCompleted    time.Duration
	RetryRPCAckTrim         time.Duration
	RetryRPCDeadlineIO      time.Duration
	RetryRPCKeepAlivePeriod time.Duration

	RetryRPCCertFilePath string
	RetryRPCKeyFilePath  string

	CheckPointInterval time.Duration

	AuthTokenCheckInterval time.Duration

	FetchNonceRangeToReturn uint64

	MinLeaseDuration       time.Duration
	LeaseInterruptInterval time.Duration
	LeaseInterruptLimit    uint32
	LeaseEvictLowLimit     uint64
	LeaseEvictHighLimit    uint64

	SwiftRetryDelay      time.Duration
	SwiftRetryExpBackoff float64
	SwiftRetryLimit      uint32

	SwiftTimeout            time.Duration
	SwiftConnectionPoolSize uint32

	ParallelObjectDeleteMax uint32

	InodeTableCacheEvictLowLimit  uint64
	InodeTableCacheEvictHighLimit uint64

	InodeTableMaxInodesPerBPlusTreePage  uint64
	RootDirMaxDirEntriesPerBPlusTreePage uint64

	LogFilePath  string // Unless starting with '/', relative to $CWD; == "" means disabled
	LogToConsole bool
	TraceEnabled bool
}

type chunkedPutContextStruct struct {
	sync.WaitGroup                      // Used to await completion of performChunkedPut goroutine
	containerName         string        //
	objectName            string        //
	buf                   []byte        //
	chunkedPutListElement *list.Element // FIFO Element of fileInodeStruct.chunkedPutList
	state                 uint8         // One of chunkedPutContextState{Open|Closing|Closed}
	pos                   int           // ObjectOffset just after last sent chunk
	sendChan              chan struct{} // Single element buffered chan to wake up *chunkedPutContextStruct.sendDaemon()
	//                                            will be closed to indicate a flush is requested
	wakeChan chan struct{} //                   Single element buffered chan to wake up *chunkedPutContextStruct.Read()
	//                                            will be closed to indicate a flush is requested
	inRead         bool //                      Set when in Read() as a hint to Close() to help Read() cleanly exit
	flushRequested bool //                      Set to remember that a flush has been requested of *chunkedPutContextStruct.Read()
}

type statsStruct struct {
	DeleteVolumeUsecs   bucketstats.BucketLog2Round // DELETE /volume/<VolumeName>
	GetConfigUsecs      bucketstats.BucketLog2Round // GET /config
	GetStatsUsecs       bucketstats.BucketLog2Round // GET /stats
	GetVersionUsecs     bucketstats.BucketLog2Round // GET /version
	GetVolumeInodeUsecs bucketstats.BucketLog2Round // GET /volume/<VolumeName>/inode/<InodeNumber>
	GetVolumeListUsecs  bucketstats.BucketLog2Round // GET /volume
	GetVolumeUsecs      bucketstats.BucketLog2Round // GET /volume/<VolumeName>
	PostVolumeUsecs     bucketstats.BucketLog2Round // POST /volume/<VolumeName>
	PutVolumeUsecs      bucketstats.BucketLog2Round // PUT /volume/<VolumeName>

	AdjustInodeTableEntryOpenCountUsecs bucketstats.BucketLog2Round // (*RetryRPCServerStruct).AdjustInodeTableEntryOpenCount()
	DeleteInodeTableEntryUsecs          bucketstats.BucketLog2Round // (*RetryRPCServerStruct).DeleteInodeTableEntry()
	FetchNonceRangeUsecs                bucketstats.BucketLog2Round // (*RetryRPCServerStruct).FetchNonceRange()
	FlushUsecs                          bucketstats.BucketLog2Round // (*RetryRPCServerStruct).Flush()
	GetInodeTableEntryUsecs             bucketstats.BucketLog2Round // (*RetryRPCServerStruct).GetInodeTableEntry()
	LeaseUsecs                          bucketstats.BucketLog2Round // (*RetryRPCServerStruct).Lease()
	MountUsecs                          bucketstats.BucketLog2Round // (*RetryRPCServerStruct).Mount()
	PutInodeTableEntriesUsecs           bucketstats.BucketLog2Round // (*RetryRPCServerStruct).PutInodeTableEntries()
	RenewMountUsecs                     bucketstats.BucketLog2Round // (*RetryRPCServerStruct).RenewMount()
	UnmountUsecs                        bucketstats.BucketLog2Round // (*RetryRPCServerStruct).Unmount()

	VolumeCheckPointUsecs bucketstats.BucketLog2Round

	AuthTokenCheckUsecs bucketstats.BucketLog2Round

	SharedLeaseRequestUsecs    bucketstats.BucketLog2Round
	PromoteLeaseRequestUsecs   bucketstats.BucketLog2Round
	ExclusiveLeaseRequestUsecs bucketstats.BucketLog2Round
	DemoteLeaseRequestUsecs    bucketstats.BucketLog2Round
	ReleaseLeaseRequestUsecs   bucketstats.BucketLog2Round

	UnmountInterrupts     bucketstats.Totaler
	DemoteLeaseInterrupts bucketstats.Totaler
	RevokeLeaseInterrupts bucketstats.Totaler

	InodeTableCacheHits   bucketstats.Totaler
	InodeTableCacheMisses bucketstats.Totaler

	SwiftObjectDeleteUsecs   bucketstats.BucketLog2Round
	SwiftObjectGetUsecs      bucketstats.BucketLog2Round
	SwiftObjectGetRangeUsecs bucketstats.BucketLog2Round
	SwiftObjectGetTailUsecs  bucketstats.BucketLog2Round
	SwiftObjectPutUsecs      bucketstats.BucketLog2Round
}

const (
	mountIDByteArrayLen = 15 // actual mountID will be Base64-encoded form
)

type leaseRequestOperationStruct struct {
	mount      *mountStruct
	inodeLease *inodeLeaseStruct
	LeaseRequestType
	replyChan chan LeaseResponseType
}

type leaseRequestStateType uint32

const (
	leaseRequestStateNone leaseRequestStateType = iota
	leaseRequestStateSharedRequested
	leaseRequestStateSharedGranted
	leaseRequestStateSharedPromoting
	leaseRequestStateSharedReleasing
	leaseRequestStateExclusiveRequested
	leaseRequestStateExclusiveGranted
	leaseRequestStateExclusiveDemoting
	leaseRequestStateExclusiveReleasing
)

type leaseRequestStruct struct {
	mount        *mountStruct
	inodeLease   *inodeLeaseStruct
	requestState leaseRequestStateType
	replyChan    chan LeaseResponseType // copied from leaseRequestOperationStruct.replyChan for LeaseRequestType == LeaseRequestType{Shared|Promote|Exclusive}
	listElement  *list.Element          // used when on one of inodeList.*List's
}

type inodeLeaseStateType uint32

const (
	inodeLeaseStateNone inodeLeaseStateType = iota
	inodeLeaseStateSharedGrantedRecently
	inodeLeaseStateSharedGrantedLongAgo
	inodeLeaseStateSharedPromoting
	inodeLeaseStateSharedReleasing
	inodeLeaseStateSharedExpired
	inodeLeaseStateExclusiveGrantedRecently
	inodeLeaseStateExclusiveGrantedLongAgo
	inodeLeaseStateExclusiveDemoting
	inodeLeaseStateExclusiveReleasing
	inodeLeaseStateExclusiveExpired
)

type inodeLeaseStruct struct {
	volume      *volumeStruct
	inodeNumber uint64
	lruElement  *list.Element // link into globals.inodeLeaseLRU
	leaseState  inodeLeaseStateType

	requestChan chan *leaseRequestOperationStruct
	stopChan    chan struct{} // closing this chan will trigger *inodeLeaseStruct.handler() to:
	//                             revoke/reject all leaseRequestStruct's in *Holder* & requestedList
	//                             issue volume.leaseHandlerWG.Done()
	//                             and exit

	sharedHoldersList    *list.List          // each list.Element.Value.(*leaseRequestStruct).requestState == leaseRequestStateSharedGranted
	promotingHolder      *leaseRequestStruct // leaseRequest.requestState == leaseRequestStateSharedPromoting
	exclusiveHolder      *leaseRequestStruct // leaseRequest.requestState == leaseRequestStateExclusiveGranted
	demotingHolder       *leaseRequestStruct // leaseRequest.requestState == leaseRequestStateExclusiveDemoting
	releasingHoldersList *list.List          // each list.Element.Value.(*leaseRequestStruct).requestState == leaseRequestState{Shared|Exclusive}Releasing
	requestedList        *list.List          // each list.Element.Value.(*leaseRequestStruct).requestState == leaseRequestState{Shared|Exclusive}Requested

	lastGrantTime     time.Time // records the time at which the last exclusive or shared holder was set/added-to exclusiveHolder/sharedHoldersList
	lastInterruptTime time.Time // records the time at which the last Interrupt was sent
	interruptsSent    uint32

	longAgoTimer   *time.Timer // if .C != nil, timing when to state transition from {Shared|Exclusive}LeaseGrantedRecently to {Shared|Exclusive}LeaseGrantedLogAgo
	interruptTimer *time.Timer // if .C != nil, timing when to issue next Interrupt... or expire a Lease
}

type mountStruct struct {
	//                                                       reentrancy covered by volumeStruct's sync.RWMutex
	volume                 *volumeStruct                  // volume.{R|}Lock() also protects each mountStruct
	mountID                string                         //
	retryRPCClientID       uint64                         //
	acceptingLeaseRequests bool                           //
	leaseRequestMap        map[uint64]*leaseRequestStruct // key == leaseRequestStruct.inodeLease.inodeNumber
	leasesExpired          bool                           // if true, leases are being expired prior to auto-deletion of mountStruct
	authTokenExpired       bool                           // if true, authToken has been rejected... needing a renewMount() to update
	authToken              string                         //
	lastAuthTime           time.Time                      // used to periodically check TTL of authToken
	listElement            *list.Element                  // LRU element on either volumeStruct.{healthy|leasesExpired|authTokenExpired}MountList
}

type inodeTableLayoutElementStruct struct {
	objectSize      uint64 // matches ilayout.InodeTableLayoutEntryV1Struct.ObjectSize
	bytesReferenced uint64 // matches ilayout.InodeTableLayoutEntryV1Struct.BytesReferenced
}

type volumeStruct struct {
	name                          string                                    //
	storageURL                    string                                    //
	authToken                     string                                    // if != "" & healthyMountList is empty, this AuthToken will be used; cleared on auth failure
	mountMap                      map[string]*mountStruct                   // key == mountStruct.mountID
	healthyMountList              *list.List                                // LRU of mountStruct's with .{leases|authToken}Expired == false
	leasesExpiredMountList        *list.List                                // list of mountStruct's with .leasesExpired == true (regardless of .authTokenExpired) value
	authTokenExpiredMountList     *list.List                                // list of mountStruct's with at .authTokenExpired == true (& .leasesExpired == false)
	deleting                      bool                                      //
	checkPoint                    *ilayout.CheckPointV1Struct               // == nil if not currently mounted and/or checkpointing
	superBlock                    *ilayout.SuperBlockV1Struct               // == nil if not currently mounted and/or checkpointing
	inodeTable                    sortedmap.BPlusTree                       // == nil if not currently mounted and/or checkpointing; key == inodeNumber; value == *ilayout.InodeTableEntryValueV1Struct
	inodeTableLayout              map[uint64]*inodeTableLayoutElementStruct // == nil if not currently mounted and/or checkpointing; key == objectNumber (matching ilayout.InodeTableLayoutEntryV1Struct.ObjectNumber)
	nextNonce                     uint64                                    // next nonce in that checkpoint reserve
	numNoncesReserved             uint64                                    // number of nonce's reserved for checkpointing
	activeDeleteObjectNumberList  *list.List                                // list of objectNumber's to be deleted since last CheckPoint
	pendingDeleteObjectNumberList *list.List                                // list of objectNumber's pending deletion after next CheckPoint
	checkPointControlChan         chan chan error                           // send chan error to chan to request a CheckPoint; close it to terminate checkPointDaemon()
	checkPointControlWG           sync.WaitGroup                            // checkPointDeamon() indicates it is done by calling .Done() on this WG
	checkPointPutObjectNumber     uint64                                    //
	checkPointPutObjectBuffer     *bytes.Buffer                             // if nil, no CheckPoint data to PUT has yet accumulated
	inodeLeaseMap                 map[uint64]*inodeLeaseStruct              // key == inodeLeaseStruct.inodeNumber
	leaseHandlerWG                sync.WaitGroup                            // .Add(1) each inodeLease insertion into inodeLeaseMap
	//                                                                         .Done() each inodeLease after it is removed from inodeLeaseMap
}

type globalsStruct struct {
	sync.Mutex                               //
	config          configStruct             //
	logFile         *os.File                 // == nil if config.LogFilePath == ""
	inodeTableCache sortedmap.BPlusTreeCache //
	inodeLeaseLRU   *list.List               // .Front() is the LRU inodeLeaseStruct.listElement
	volumeMap       sortedmap.LLRBTree       // key == volumeStruct.name; value == *volumeStruct
	mountMap        map[string]*mountStruct  // key == mountStruct.mountID
	httpClient      *http.Client             //
	retryrpcServer  *retryrpc.Server         //
	httpServer      *http.Server             //
	httpServerWG    sync.WaitGroup           //
	stats           *statsStruct             //
}

var globals globalsStruct

func initializeGlobals(confMap conf.ConfMap) (err error) {
	var (
		configJSONified string
	)

	// Default logging related globals

	globals.config.LogFilePath = ""
	globals.config.LogToConsole = true
	globals.logFile = nil

	// Process resultant confMap

	globals.config.PublicIPAddr, err = confMap.FetchOptionValueString("IMGR", "PublicIPAddr")
	if nil != err {
		logFatal(err)
	}
	globals.config.PrivateIPAddr, err = confMap.FetchOptionValueString("IMGR", "PrivateIPAddr")
	if nil != err {
		logFatal(err)
	}
	globals.config.RetryRPCPort, err = confMap.FetchOptionValueUint16("IMGR", "RetryRPCPort")
	if nil != err {
		logFatal(err)
	}
	globals.config.HTTPServerPort, err = confMap.FetchOptionValueUint16("IMGR", "HTTPServerPort")
	if nil != err {
		logFatal(err)
	}
	globals.config.HTTPServerPort, err = confMap.FetchOptionValueUint16("IMGR", "HTTPServerPort")
	if nil != err {
		logFatal(err)
	}

	globals.config.RetryRPCTTLCompleted, err = confMap.FetchOptionValueDuration("IMGR", "RetryRPCTTLCompleted")
	if nil != err {
		logFatal(err)
	}
	globals.config.RetryRPCAckTrim, err = confMap.FetchOptionValueDuration("IMGR", "RetryRPCAckTrim")
	if nil != err {
		logFatal(err)
	}
	globals.config.RetryRPCDeadlineIO, err = confMap.FetchOptionValueDuration("IMGR", "RetryRPCDeadlineIO")
	if nil != err {
		logFatal(err)
	}
	globals.config.RetryRPCKeepAlivePeriod, err = confMap.FetchOptionValueDuration("IMGR", "RetryRPCKeepAlivePeriod")
	if nil != err {
		logFatal(err)
	}

	err = confMap.VerifyOptionIsMissing("IMGR", "RetryRPCCertFilePath")
	if nil == err {
		err = confMap.VerifyOptionIsMissing("IMGR", "RetryRPCKeyFilePath")
		if nil == err {
			globals.config.RetryRPCCertFilePath = ""
			globals.config.RetryRPCKeyFilePath = ""
		} else {
			err = confMap.VerifyOptionValueIsEmpty("IMGR", "RetryRPCKeyFilePath")
			if nil == err {
				globals.config.RetryRPCCertFilePath = ""
				globals.config.RetryRPCKeyFilePath = ""
			} else {
				err = fmt.Errorf("[IMGR]RetryRPCCertFilePath is missing but [IMGR]RetryRPCKeyFilePath is present and non-empty")
				logFatal(err)
			}
		}
	} else {
		err = confMap.VerifyOptionValueIsEmpty("IMGR", "RetryRPCCertFilePath")
		if nil == err {
			err = confMap.VerifyOptionIsMissing("IMGR", "RetryRPCKeyFilePath")
			if nil == err {
				globals.config.RetryRPCCertFilePath = ""
				globals.config.RetryRPCKeyFilePath = ""
			} else {
				err = confMap.VerifyOptionValueIsEmpty("IMGR", "RetryRPCKeyFilePath")
				if nil == err {
					globals.config.RetryRPCCertFilePath = ""
					globals.config.RetryRPCKeyFilePath = ""
				} else {
					err = fmt.Errorf("[IMGR]RetryRPCCertFilePath is present and empty but [IMGR]RetryRPCKeyFilePath is present and non-empty")
					logFatal(err)
				}
			}
		} else {
			globals.config.RetryRPCCertFilePath, err = confMap.FetchOptionValueString("IMGR", "RetryRPCCertFilePath")
			if nil != err {
				logFatal(err)
			} else {
				globals.config.RetryRPCKeyFilePath, err = confMap.FetchOptionValueString("IMGR", "RetryRPCKeyFilePath")
				if nil != err {
					logFatal(err)
				}
			}
		}
	}

	globals.config.CheckPointInterval, err = confMap.FetchOptionValueDuration("IMGR", "CheckPointInterval")
	if nil != err {
		logFatal(err)
	}

	globals.config.AuthTokenCheckInterval, err = confMap.FetchOptionValueDuration("IMGR", "AuthTokenCheckInterval")
	if nil != err {
		logFatal(err)
	}

	globals.config.FetchNonceRangeToReturn, err = confMap.FetchOptionValueUint64("IMGR", "FetchNonceRangeToReturn")
	if nil != err {
		logFatal(err)
	}

	globals.config.MinLeaseDuration, err = confMap.FetchOptionValueDuration("IMGR", "MinLeaseDuration")
	if nil != err {
		logFatal(err)
	}
	globals.config.LeaseInterruptInterval, err = confMap.FetchOptionValueDuration("IMGR", "LeaseInterruptInterval")
	if nil != err {
		logFatal(err)
	}
	globals.config.LeaseInterruptLimit, err = confMap.FetchOptionValueUint32("IMGR", "LeaseInterruptLimit")
	if nil != err {
		logFatal(err)
	}
	globals.config.LeaseEvictLowLimit, err = confMap.FetchOptionValueUint64("IMGR", "LeaseEvictLowLimit")
	if nil != err {
		logFatal(err)
	}
	globals.config.LeaseEvictHighLimit, err = confMap.FetchOptionValueUint64("IMGR", "LeaseEvictHighLimit")
	if nil != err {
		logFatal(err)
	}

	globals.config.SwiftRetryDelay, err = confMap.FetchOptionValueDuration("IMGR", "SwiftRetryDelay")
	if nil != err {
		logFatal(err)
	}
	globals.config.SwiftRetryExpBackoff, err = confMap.FetchOptionValueFloat64("IMGR", "SwiftRetryExpBackoff")
	if nil != err {
		logFatal(err)
	}
	globals.config.SwiftRetryLimit, err = confMap.FetchOptionValueUint32("IMGR", "SwiftRetryLimit")
	if nil != err {
		logFatal(err)
	}

	globals.config.SwiftTimeout, err = confMap.FetchOptionValueDuration("IMGR", "SwiftTimeout")
	if nil != err {
		logFatal(err)
	}
	globals.config.SwiftConnectionPoolSize, err = confMap.FetchOptionValueUint32("IMGR", "SwiftConnectionPoolSize")
	if nil != err {
		logFatal(err)
	}

	globals.config.ParallelObjectDeleteMax, err = confMap.FetchOptionValueUint32("IMGR", "ParallelObjectDeleteMax")
	if nil != err {
		logFatal(err)
	}

	globals.config.InodeTableCacheEvictLowLimit, err = confMap.FetchOptionValueUint64("IMGR", "InodeTableCacheEvictLowLimit")
	if nil != err {
		logFatal(err)
	}
	globals.config.InodeTableCacheEvictHighLimit, err = confMap.FetchOptionValueUint64("IMGR", "InodeTableCacheEvictHighLimit")
	if nil != err {
		logFatal(err)
	}

	globals.config.InodeTableMaxInodesPerBPlusTreePage, err = confMap.FetchOptionValueUint64("IMGR", "InodeTableMaxInodesPerBPlusTreePage")
	if nil != err {
		logFatal(err)
	}
	globals.config.RootDirMaxDirEntriesPerBPlusTreePage, err = confMap.FetchOptionValueUint64("IMGR", "RootDirMaxDirEntriesPerBPlusTreePage")
	if nil != err {
		logFatal(err)
	}

	globals.config.LogFilePath, err = confMap.FetchOptionValueString("IMGR", "LogFilePath")
	if nil != err {
		err = confMap.VerifyOptionValueIsEmpty("IMGR", "LogFilePath")
		if nil == err {
			globals.config.LogFilePath = ""
		} else {
			logFatalf("[IMGR]LogFilePath must either be a valid string or empty]")
		}
	}
	globals.config.LogToConsole, err = confMap.FetchOptionValueBool("IMGR", "LogToConsole")
	if nil != err {
		logFatal(err)
	}
	globals.config.TraceEnabled, err = confMap.FetchOptionValueBool("IMGR", "TraceEnabled")
	if nil != err {
		logFatal(err)
	}

	configJSONified = utils.JSONify(globals.config, true)

	logInfof("globals.config:\n%s", configJSONified)

	globals.stats = &statsStruct{}

	bucketstats.Register("IMGR", "", globals.stats)

	err = nil
	return
}

func uninitializeGlobals() (err error) {
	globals.config.PrivateIPAddr = ""
	globals.config.PublicIPAddr = ""
	globals.config.RetryRPCPort = 0
	globals.config.HTTPServerPort = 0

	globals.config.RetryRPCTTLCompleted = time.Duration(0)
	globals.config.RetryRPCAckTrim = time.Duration(0)
	globals.config.RetryRPCDeadlineIO = time.Duration(0)
	globals.config.RetryRPCKeepAlivePeriod = time.Duration(0)

	globals.config.RetryRPCCertFilePath = ""
	globals.config.RetryRPCKeyFilePath = ""

	globals.config.CheckPointInterval = time.Duration(0)

	globals.config.AuthTokenCheckInterval = time.Duration(0)

	globals.config.FetchNonceRangeToReturn = 0

	globals.config.MinLeaseDuration = time.Duration(0)
	globals.config.LeaseInterruptInterval = time.Duration(0)
	globals.config.LeaseInterruptLimit = 0
	globals.config.LeaseEvictLowLimit = 0
	globals.config.LeaseEvictHighLimit = 0

	globals.config.SwiftRetryDelay = time.Duration(0)
	globals.config.SwiftRetryExpBackoff = 0.0
	globals.config.SwiftRetryLimit = 0

	globals.config.SwiftTimeout = time.Duration(0)
	globals.config.SwiftConnectionPoolSize = 0

	globals.config.ParallelObjectDeleteMax = 0

	globals.config.InodeTableCacheEvictLowLimit = 0
	globals.config.InodeTableCacheEvictHighLimit = 0

	globals.config.InodeTableMaxInodesPerBPlusTreePage = 0
	globals.config.RootDirMaxDirEntriesPerBPlusTreePage = 0

	globals.config.LogFilePath = ""
	globals.config.LogToConsole = false
	globals.config.TraceEnabled = false

	bucketstats.UnRegister("IMGR", "")

	err = nil
	return
}
