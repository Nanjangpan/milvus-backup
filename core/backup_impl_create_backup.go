package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
	"go.uber.org/zap"

	"github.com/zilliztech/milvus-backup/core/proto/backuppb"
	"github.com/zilliztech/milvus-backup/core/utils"
	"github.com/zilliztech/milvus-backup/internal/common"
	"github.com/zilliztech/milvus-backup/internal/log"
)

func (b *BackupContext) CreateBackup(ctx context.Context, request *backuppb.CreateBackupRequest) *backuppb.BackupInfoResponse {
	if request.GetRequestId() == "" {
		request.RequestId = utils.UUID()
	}
	log.Info("receive CreateBackupRequest",
		zap.String("requestId", request.GetRequestId()),
		zap.String("backupName", request.GetBackupName()),
		zap.Strings("collections", request.GetCollectionNames()),
		zap.String("databaseCollections", utils.GetCreateDBCollections(request)),
		zap.Bool("async", request.GetAsync()))

	resp := &backuppb.BackupInfoResponse{
		RequestId: request.GetRequestId(),
	}

	if !b.started {
		err := b.Start()
		if err != nil {
			resp.Code = backuppb.ResponseCode_Fail
			resp.Msg = err.Error()
			return resp
		}
	}

	// backup name validate
	if request.GetBackupName() == "" {
		request.BackupName = "backup_" + fmt.Sprint(time.Now().UTC().Format("2006_01_02_15_04_05_")) + fmt.Sprint(time.Now().Nanosecond())
	}
	if request.GetBackupName() != "" {
		exist, err := b.getStorageClient().Exist(b.ctx, b.backupBucketName, b.backupRootPath+SEPERATOR+request.GetBackupName())
		if err != nil {
			errMsg := fmt.Sprintf("fail to check whether exist backup with name: %s", request.GetBackupName())
			log.Error(errMsg, zap.Error(err))
			resp.Code = backuppb.ResponseCode_Fail
			resp.Msg = errMsg + "/n" + err.Error()
			return resp
		}
		if exist {
			errMsg := fmt.Sprintf("backup already exist with the name: %s", request.GetBackupName())
			log.Error(errMsg)
			resp.Code = backuppb.ResponseCode_Parameter_Error
			resp.Msg = errMsg
			return resp
		}
	}
	err := utils.ValidateType(request.GetBackupName(), BACKUP_NAME)
	if err != nil {
		log.Error("illegal backup name", zap.Error(err))
		resp.Code = backuppb.ResponseCode_Parameter_Error
		resp.Msg = err.Error()
		return resp
	}

	var name string = request.BackupName

	milvusVersion, err := b.getMilvusClient().GetVersion(b.ctx)
	if err != nil {
		log.Error("fail to get milvus version", zap.Error(err))
		resp.Code = backuppb.ResponseCode_Fail
		resp.Msg = err.Error()
		return resp
	}

	backup := &backuppb.BackupInfo{
		Id:            request.GetRequestId(),
		StateCode:     backuppb.BackupTaskStateCode_BACKUP_INITIAL,
		StartTime:     time.Now().UnixNano() / int64(time.Millisecond),
		Name:          name,
		MilvusVersion: milvusVersion,
	}
	b.backupTasks.Store(request.GetRequestId(), backup)
	b.backupNameIdDict.Store(name, request.GetRequestId())

	if request.Async {
		go b.executeCreateBackup(ctx, request, backup)
		asyncResp := &backuppb.BackupInfoResponse{
			RequestId: request.GetRequestId(),
			Code:      backuppb.ResponseCode_Success,
			Msg:       "create backup is executing asynchronously",
			Data:      backup,
		}
		return asyncResp
	} else {
		task, err := b.executeCreateBackup(ctx, request, backup)
		resp.Data = task
		if err != nil {
			resp.Code = backuppb.ResponseCode_Fail
			resp.Msg = err.Error()
		} else {
			resp.Code = backuppb.ResponseCode_Success
			resp.Msg = "success"
		}
		return resp
	}
}

func (b *BackupContext) refreshBackupMeta(id string, backupInfo *backuppb.BackupInfo, leveledBackupInfo *LeveledBackupInfo) (*backuppb.BackupInfo, error) {
	log.Debug("call refreshBackupMeta", zap.String("id", id))
	backup, err := levelToTree(leveledBackupInfo)
	if err != nil {
		return backupInfo, err
	}
	b.backupTasks.Store(id, backup)
	backupInfo = backup
	return backup, nil
}

func (b *BackupContext) refreshBackupCache(backupInfo *backuppb.BackupInfo) {
	log.Debug("refreshBackupCache", zap.String("id", backupInfo.GetId()))
	b.backupTasks.Store(backupInfo.GetId(), backupInfo)
}

type collectionStruct struct {
	db             string
	collectionName string
}

// parse collections to backup
// For backward compatibility：
//   1，parse dbCollections first,
//   2，if dbCollections not set, use collectionNames
func (b *BackupContext) parseBackupCollections(request *backuppb.CreateBackupRequest) ([]collectionStruct, error) {
	log.Debug("Request collection names",
		zap.Strings("request_collection_names", request.GetCollectionNames()),
		zap.String("request_db_collections", utils.GetCreateDBCollections(request)),
		zap.Int("length", len(request.GetCollectionNames())))
	var toBackupCollections []collectionStruct

	dbCollectionsStr := utils.GetCreateDBCollections(request)
	// first priority: dbCollections
	if dbCollectionsStr != "" {
		var dbCollections DbCollections
		err := jsoniter.UnmarshalFromString(dbCollectionsStr, &dbCollections)
		if err != nil {
			log.Error("fail in unmarshal dbCollections in CreateBackupRequest", zap.String("dbCollections", dbCollectionsStr), zap.Error(err))
			return nil, err
		}
		for db, collections := range dbCollections {
			if len(collections) == 0 {
				collections, err := b.getMilvusClient().ListCollections(b.ctx, db)
				if err != nil {
					log.Error("fail in ListCollections", zap.Error(err))
					return nil, err
				}
				for _, coll := range collections {
					log.Debug("Add collection to toBackupCollections", zap.String("db", db), zap.String("collection", coll.Name))
					toBackupCollections = append(toBackupCollections, collectionStruct{db, coll.Name})
				}
			} else {
				for _, coll := range collections {
					toBackupCollections = append(toBackupCollections, collectionStruct{db, coll})
				}
			}
		}
		log.Debug("Parsed backup collections from request.db_collections", zap.Int("length", len(toBackupCollections)))
		return toBackupCollections, nil
	}

	if request.GetCollectionNames() == nil || len(request.GetCollectionNames()) == 0 {
		dbs, err := b.getMilvusClient().ListDatabases(b.ctx)
		if err != nil {
			log.Error("fail in ListDatabases", zap.Error(err))
			return nil, err
		}
		for _, db := range dbs {
			collections, err := b.getMilvusClient().ListCollections(b.ctx, db.Name)
			if err != nil {
				log.Error("fail in ListCollections", zap.Error(err))
				return nil, err
			}
			for _, coll := range collections {
				toBackupCollections = append(toBackupCollections, collectionStruct{db.Name, coll.Name})
			}
		}
		log.Debug(fmt.Sprintf("List %v collections", len(toBackupCollections)))
	} else {
		for _, collectionName := range request.GetCollectionNames() {
			var dbName = "default"
			if strings.Contains(collectionName, ".") {
				splits := strings.Split(collectionName, ".")
				dbName = splits[0]
				collectionName = splits[1]
			}

			exist, err := b.getMilvusClient().HasCollection(b.ctx, dbName, collectionName)
			if err != nil {
				log.Error("fail in HasCollection", zap.Error(err))
				return nil, err
			}
			if !exist {
				errMsg := fmt.Sprintf("request backup collection does not exist: %s.%s", dbName, collectionName)
				log.Error(errMsg)
				return nil, errors.New(errMsg)
			}
			toBackupCollections = append(toBackupCollections, collectionStruct{dbName, collectionName})
		}
	}

	return toBackupCollections, nil
}

func (b *BackupContext) backupCollection(ctx context.Context, backupInfo *backuppb.BackupInfo, collection collectionStruct, force bool) error {
	log.Info("start backup collection", zap.String("db", collection.db), zap.String("collection", collection.collectionName))
	// list collection result is not complete
	completeCollection, err := b.getMilvusClient().DescribeCollection(b.ctx, collection.db, collection.collectionName)
	if err != nil {
		log.Error("fail in DescribeCollection", zap.Error(err))
		return err
	}
	fields := make([]*backuppb.FieldSchema, 0)
	for _, field := range completeCollection.Schema.Fields {
		fields = append(fields, &backuppb.FieldSchema{
			FieldID:        field.ID,
			Name:           field.Name,
			IsPrimaryKey:   field.PrimaryKey,
			Description:    field.Description,
			AutoID:         field.AutoID,
			DataType:       backuppb.DataType(field.DataType),
			TypeParams:     utils.MapToKVPair(field.TypeParams),
			IndexParams:    utils.MapToKVPair(field.IndexParams),
			IsDynamic:      field.IsDynamic,
			IsPartitionKey: field.IsPartitionKey,
			ElementType:    backuppb.DataType(field.ElementType),
		})
	}
	schema := &backuppb.CollectionSchema{
		Name:               completeCollection.Schema.CollectionName,
		Description:        completeCollection.Schema.Description,
		AutoID:             completeCollection.Schema.AutoID,
		Fields:             fields,
		EnableDynamicField: completeCollection.Schema.EnableDynamicField,
	}

	indexInfos := make([]*backuppb.IndexInfo, 0)
	indexDict := make(map[string]*backuppb.IndexInfo, 0)
	log.Info("try to get index",
		zap.String("collection_name", completeCollection.Name))
	for _, field := range completeCollection.Schema.Fields {
		//if field.DataType != entity.FieldTypeBinaryVector && field.DataType != entity.FieldTypeFloatVector {
		//	continue
		//}
		fieldIndex, err := b.getMilvusClient().DescribeIndex(b.ctx, collection.db, completeCollection.Name, field.Name)
		if err != nil {
			if strings.Contains(err.Error(), "index not found") ||
				strings.HasPrefix(err.Error(), "index doesn't exist") {
				// todo
				log.Info("field has no index",
					zap.String("collection_name", completeCollection.Name),
					zap.String("field_name", field.Name))
				continue
			} else {
				log.Error("fail in DescribeIndex", zap.Error(err))
				return err
			}
		}
		log.Info("field index",
			zap.String("collection_name", completeCollection.Name),
			zap.String("field_name", field.Name),
			zap.Any("index info", fieldIndex))
		for _, index := range fieldIndex {
			if _, ok := indexDict[index.Name()]; ok {
				continue
			} else {
				indexInfo := &backuppb.IndexInfo{
					FieldName: index.FieldName(),
					IndexName: index.Name(),
					IndexType: string(index.IndexType()),
					Params:    index.Params(),
				}
				indexInfos = append(indexInfos, indexInfo)
				indexDict[index.Name()] = indexInfo
			}
		}
	}

	collectionBackup := &backuppb.CollectionBackupInfo{
		Id:               utils.UUID(),
		StateCode:        backuppb.BackupTaskStateCode_BACKUP_INITIAL,
		StartTime:        time.Now().Unix(),
		CollectionId:     completeCollection.ID,
		DbName:           collection.db, // todo currently db_name is not used in many places
		CollectionName:   completeCollection.Name,
		Schema:           schema,
		ShardsNum:        completeCollection.ShardNum,
		ConsistencyLevel: backuppb.ConsistencyLevel(completeCollection.ConsistencyLevel),
		HasIndex:         len(indexInfos) > 0,
		IndexInfos:       indexInfos,
	}
	backupInfo.CollectionBackups = append(backupInfo.CollectionBackups, collectionBackup)

	b.refreshBackupCache(backupInfo)
	partitionBackupInfos := make([]*backuppb.PartitionBackupInfo, 0)
	partitions, err := b.getMilvusClient().ShowPartitions(b.ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName())
	if err != nil {
		log.Error("fail to ShowPartitions", zap.Error(err))
		return err
	}

	// use GetLoadingProgress currently, GetLoadState is a new interface @20230104  milvus pr#21515
	collectionLoadProgress, err := b.getMilvusClient().GetLoadingProgress(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName(), []string{})
	if err != nil {
		log.Error("fail to GetLoadingProgress of collection", zap.Error(err))
		return err
	}

	var collectionLoadState string
	partitionLoadStates := make(map[string]string, 0)
	if collectionLoadProgress == 0 {
		collectionLoadState = LoadState_NotLoad
		for _, partition := range partitions {
			partitionLoadStates[partition.Name] = LoadState_NotLoad
		}
	} else if collectionLoadProgress == 100 {
		collectionLoadState = LoadState_Loaded
		for _, partition := range partitions {
			partitionLoadStates[partition.Name] = LoadState_Loaded
		}
	} else {
		collectionLoadState = LoadState_Loading
		for _, partition := range partitions {
			loadProgress, err := b.getMilvusClient().GetLoadingProgress(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName(), []string{partition.Name})
			if err != nil {
				log.Error("fail to GetLoadingProgress of partition", zap.Error(err))
				return err
			}
			if loadProgress == 0 {
				partitionLoadStates[partition.Name] = LoadState_NotLoad
			} else if loadProgress == 100 {
				partitionLoadStates[partition.Name] = LoadState_Loaded
			} else {
				partitionLoadStates[partition.Name] = LoadState_Loading
			}
		}
	}

	// fill segments
	filledSegments := make([]*entity.Segment, 0)
	if !force {
		// Flush
		segmentEntitiesBeforeFlush, err := b.getMilvusClient().GetPersistentSegmentInfo(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName())
		if err != nil {
			return err
		}
		log.Info("GetPersistentSegmentInfo before flush from milvus",
			zap.String("collectionName", collectionBackup.GetCollectionName()),
			zap.Int("segmentNumBeforeFlush", len(segmentEntitiesBeforeFlush)))
		newSealedSegmentIDs, flushedSegmentIDs, timeOfSeal, err := b.getMilvusClient().FlushV2(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName(), false)
		if err != nil {
			log.Error(fmt.Sprintf("fail to flush the collection: %s", collectionBackup.GetCollectionName()))
			return err
		}
		log.Info("flush segments",
			zap.String("collectionName", collectionBackup.GetCollectionName()),
			zap.Int64s("newSealedSegmentIDs", newSealedSegmentIDs),
			zap.Int64s("flushedSegmentIDs", flushedSegmentIDs),
			zap.Int64("timeOfSeal", timeOfSeal))
		collectionBackup.BackupTimestamp = utils.ComposeTS(timeOfSeal, 0)
		collectionBackup.BackupPhysicalTimestamp = uint64(timeOfSeal)

		flushSegmentIDs := append(newSealedSegmentIDs, flushedSegmentIDs...)
		segmentEntitiesAfterFlush, err := b.getMilvusClient().GetPersistentSegmentInfo(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName())
		if err != nil {
			return err
		}
		log.Info("GetPersistentSegmentInfo after flush from milvus",
			zap.String("collectionName", collectionBackup.GetCollectionName()),
			zap.Int("segmentNumBeforeFlush", len(segmentEntitiesBeforeFlush)),
			zap.Int("segmentNumAfterFlush", len(segmentEntitiesAfterFlush)))
		segmentDict := utils.ArrayToMap(flushSegmentIDs)
		for _, seg := range segmentEntitiesAfterFlush {
			sid := seg.ID
			if _, ok := segmentDict[sid]; ok {
				delete(segmentDict, sid)
				filledSegments = append(filledSegments, seg)
			} else {
				log.Debug("this may be new segments after flush, skip it", zap.Int64("id", sid))
			}
		}
		for _, seg := range segmentEntitiesBeforeFlush {
			sid := seg.ID
			if _, ok := segmentDict[sid]; ok {
				delete(segmentDict, sid)
				filledSegments = append(filledSegments, seg)
			} else {
				log.Debug("this may be old segments before flush, skip it", zap.Int64("id", sid))
			}
		}
		if len(segmentDict) > 0 {
			// very rare situation, segments return in flush doesn't exist in either segmentEntitiesBeforeFlush and segmentEntitiesAfterFlush
			errorMsg := "Segment return in Flush not exist in GetPersistentSegmentInfo. segment ids: " + fmt.Sprint(utils.MapKeyArray(segmentDict))
			log.Warn(errorMsg)
		}
	} else {
		// Flush
		segmentEntitiesBeforeFlush, err := b.getMilvusClient().GetPersistentSegmentInfo(ctx, collectionBackup.GetDbName(), collectionBackup.GetCollectionName())
		if err != nil {
			return err
		}
		log.Info("GetPersistentSegmentInfo from milvus",
			zap.String("collectionName", collectionBackup.GetCollectionName()),
			zap.Int("segmentNum", len(segmentEntitiesBeforeFlush)))
		for _, seg := range segmentEntitiesBeforeFlush {
			filledSegments = append(filledSegments, seg)
		}
	}

	if err != nil {
		collectionBackup.StateCode = backuppb.BackupTaskStateCode_BACKUP_FAIL
		collectionBackup.ErrorMessage = err.Error()
		return err
	}
	log.Info("Finished fill segment",
		zap.String("collectionName", collectionBackup.GetCollectionName()))

	log.Info("reading SegmentInfos from storage, this may cost several minutes if data is large",
		zap.String("collectionName", collectionBackup.GetCollectionName()))
	segmentBackupInfos := make([]*backuppb.SegmentBackupInfo, 0)
	partSegInfoMap := make(map[int64][]*backuppb.SegmentBackupInfo)
	mu := sync.Mutex{}
	wp, err := common.NewWorkerPool(ctx, b.params.BackupCfg.BackupCopyDataParallelism, RPS)
	if err != nil {
		return err
	}
	wp.Start()
	for _, v := range filledSegments {
		segment := v
		job := func(ctx context.Context) error {
			segmentInfo, err := b.readSegmentInfo(ctx, segment.CollectionID, segment.ParititionID, segment.ID, segment.NumRows)
			if err != nil {
				return err
			}
			if len(segmentInfo.Binlogs) == 0 {
				log.Warn("this segment has no insert binlog", zap.Int64("id", segment.ID))
			}
			mu.Lock()
			partSegInfoMap[segment.ParititionID] = append(partSegInfoMap[segment.ParititionID], segmentInfo)
			segmentBackupInfos = append(segmentBackupInfos, segmentInfo)
			mu.Unlock()
			return nil
		}
		wp.Submit(job)
	}
	wp.Done()
	if err := wp.Wait(); err != nil {
		return err
	}
	log.Info("readSegmentInfo from storage",
		zap.String("collectionName", collectionBackup.GetCollectionName()),
		zap.Int("segmentNum", len(filledSegments)))

	for _, partition := range partitions {
		partitionSegments := partSegInfoMap[partition.ID]
		var size int64 = 0
		for _, seg := range partitionSegments {
			size += seg.GetSize()
		}
		partitionBackupInfo := &backuppb.PartitionBackupInfo{
			PartitionId:    partition.ID,
			PartitionName:  partition.Name,
			CollectionId:   collectionBackup.GetCollectionId(),
			SegmentBackups: partSegInfoMap[partition.ID],
			Size:           size,
			LoadState:      partitionLoadStates[partition.Name],
		}
		partitionBackupInfos = append(partitionBackupInfos, partitionBackupInfo)
		//partitionLevelBackupInfos = append(partitionLevelBackupInfos, partitionBackupInfo)
	}

	//leveledBackupInfo.partitionLevel = &backuppb.PartitionLevelBackupInfo{
	//	Infos: partitionLevelBackupInfos,
	//}
	collectionBackup.PartitionBackups = partitionBackupInfos
	collectionBackup.LoadState = collectionLoadState
	b.refreshBackupCache(backupInfo)
	log.Info("finish build partition info",
		zap.String("collectionName", collectionBackup.GetCollectionName()),
		zap.Int("partitionNum", len(partitionBackupInfos)))

	log.Info("Begin copy data",
		zap.String("collectionName", collectionBackup.GetCollectionName()),
		zap.Int("segmentNum", len(segmentBackupInfos)))

	var collectionBackupSize int64 = 0
	for _, part := range partitionBackupInfos {
		collectionBackupSize += part.GetSize()
		if part.GetSize() > b.params.BackupCfg.MaxSegmentGroupSize {
			log.Info("partition size is larger than MaxSegmentGroupSize, will separate segments into groups in backup files",
				zap.Int64("collectionId", part.GetCollectionId()),
				zap.Int64("partitionId", part.GetPartitionId()),
				zap.Int64("partitionSize", part.GetSize()),
				zap.Int64("MaxSegmentGroupSize", b.params.BackupCfg.MaxSegmentGroupSize))
			segments := partSegInfoMap[part.GetPartitionId()]
			var bufferSize int64 = 0
			// 0 is illegal value, start from 1
			var segGroupID int64 = 1
			for _, seg := range segments {
				if seg.Size > b.params.BackupCfg.MaxSegmentGroupSize && bufferSize == 0 {
					seg.GroupId = segGroupID
					segGroupID = segGroupID + 1
				} else if bufferSize+seg.Size > b.params.BackupCfg.MaxSegmentGroupSize {
					segGroupID = segGroupID + 1
					seg.GroupId = segGroupID
					bufferSize = 0
					bufferSize = bufferSize + seg.Size
				} else {
					seg.GroupId = segGroupID
					bufferSize = bufferSize + seg.Size
				}
			}
		} else {
			log.Info("partition size is smaller than MaxSegmentGroupSize, won't separate segments into groups in backup files",
				zap.Int64("collectionId", part.GetCollectionId()),
				zap.Int64("partitionId", part.GetPartitionId()),
				zap.Int64("partitionSize", part.GetSize()),
				zap.Int64("MaxSegmentGroupSize", b.params.BackupCfg.MaxSegmentGroupSize))
		}
	}

	sort.SliceStable(segmentBackupInfos, func(i, j int) bool {
		return segmentBackupInfos[i].Size < segmentBackupInfos[j].Size
	})
	err = b.copySegments(ctx, segmentBackupInfos, BackupBinlogDirPath(b.backupRootPath, backupInfo.GetName()))
	if err != nil {
		return err
	}
	b.refreshBackupCache(backupInfo)

	collectionBackup.Size = collectionBackupSize
	collectionBackup.EndTime = time.Now().Unix()
	return nil
}

func (b *BackupContext) executeCreateBackup(ctx context.Context, request *backuppb.CreateBackupRequest, backupInfo *backuppb.BackupInfo) (*backuppb.BackupInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	wp, err := common.NewWorkerPool(ctx, b.params.BackupCfg.BackupParallelism, RPS)
	if err != nil {
		return backupInfo, err
	}
	wp.Start()
	log.Info("Start collection level backup pool", zap.Int("parallelism", b.params.BackupCfg.BackupParallelism))

	backupInfo.BackupTimestamp = uint64(time.Now().UnixNano() / int64(time.Millisecond))
	backupInfo.StateCode = backuppb.BackupTaskStateCode_BACKUP_EXECUTING

	defer b.refreshBackupCache(backupInfo)

	// 1, get collection level meta
	toBackupCollections, err := b.parseBackupCollections(request)
	if err != nil {
		log.Error("parse backup collections from request failed", zap.Error(err))
		return backupInfo, err
	}
	collectionNames := make([]string, len(toBackupCollections))
	for i, coll := range toBackupCollections {
		collectionNames[i] = coll.collectionName
	}
	log.Info("collections to backup", zap.Strings("collections", collectionNames))

	for _, collection := range toBackupCollections {
		collectionClone := collection
		job := func(ctx context.Context) error {
			err := b.backupCollection(ctx, backupInfo, collectionClone, request.GetForce())
			return err
		}
		wp.Submit(job)
	}
	wp.Done()
	if err := wp.Wait(); err != nil {
		return backupInfo, err
	}

	var backupSize int64 = 0
	leveledBackupInfo, err := treeToLevel(backupInfo)
	if err != nil {
		return backupInfo, err
	}
	for _, coll := range leveledBackupInfo.collectionLevel.GetInfos() {
		backupSize += coll.GetSize()
	}
	backupInfo.Size = backupSize
	backupInfo.EndTime = time.Now().UnixNano() / int64(time.Millisecond)
	backupInfo.StateCode = backuppb.BackupTaskStateCode_BACKUP_SUCCESS
	b.refreshBackupCache(backupInfo)

	// 7, write meta data
	output, _ := serialize(backupInfo)
	log.Debug("backup meta", zap.String("value", string(output.BackupMetaBytes)))
	log.Debug("collection meta", zap.String("value", string(output.CollectionMetaBytes)))
	log.Debug("partition meta", zap.String("value", string(output.PartitionMetaBytes)))
	log.Debug("segment meta", zap.String("value", string(output.SegmentMetaBytes)))

	b.getStorageClient().Write(ctx, b.backupBucketName, BackupMetaPath(b.backupRootPath, backupInfo.GetName()), output.BackupMetaBytes)
	b.getStorageClient().Write(ctx, b.backupBucketName, CollectionMetaPath(b.backupRootPath, backupInfo.GetName()), output.CollectionMetaBytes)
	b.getStorageClient().Write(ctx, b.backupBucketName, PartitionMetaPath(b.backupRootPath, backupInfo.GetName()), output.PartitionMetaBytes)
	b.getStorageClient().Write(ctx, b.backupBucketName, SegmentMetaPath(b.backupRootPath, backupInfo.GetName()), output.SegmentMetaBytes)
	b.getStorageClient().Write(ctx, b.backupBucketName, FullMetaPath(b.backupRootPath, backupInfo.GetName()), output.FullMetaBytes)

	log.Info("finish executeCreateBackup",
		zap.String("requestId", request.GetRequestId()),
		zap.String("backupName", request.GetBackupName()),
		zap.Strings("collections", request.GetCollectionNames()),
		zap.Bool("async", request.GetAsync()),
		zap.String("backup meta", string(output.BackupMetaBytes)))
	return backupInfo, nil
}

func (b *BackupContext) copySegments(ctx context.Context, segments []*backuppb.SegmentBackupInfo, dstPath string) error {
	wp, err := common.NewWorkerPool(ctx, b.params.BackupCfg.BackupCopyDataParallelism, RPS)
	if err != nil {
		return err
	}
	wp.Start()

	// generate target path
	// milvus_rootpath/insert_log/collection_id/partition_id/segment_id/ =>
	// backup_rootpath/backup_name/binlog/insert_log/collection_id/partition_id/group_id/segment_id
	backupPathFunc := func(binlogPath, rootPath, backupBinlogPath string) string {
		if rootPath == "" {
			return dstPath + SEPERATOR + binlogPath
		} else {
			return strings.Replace(binlogPath, rootPath, dstPath, 1)
		}
	}

	for _, segment := range segments {
		start := time.Now().Unix()
		log.Debug("copy segment",
			zap.Int64("collection_id", segment.GetCollectionId()),
			zap.Int64("partition_id", segment.GetPartitionId()),
			zap.Int64("segment_id", segment.GetSegmentId()),
			zap.Int64("group_id", segment.GetGroupId()),
			zap.Int64("size", segment.GetSize()))
		// insert log
		for _, binlogs := range segment.GetBinlogs() {
			for _, binlog := range binlogs.GetBinlogs() {
				targetPath := backupPathFunc(binlog.GetLogPath(), b.milvusRootPath, dstPath)
				if segment.GetGroupId() != 0 {
					targetPath = strings.Replace(targetPath,
						strconv.FormatInt(segment.GetPartitionId(), 10),
						strconv.FormatInt(segment.GetPartitionId(), 10)+"/"+strconv.FormatInt(segment.GetGroupId(), 10),
						1)
				}
				if targetPath == binlog.GetLogPath() {
					return errors.New(fmt.Sprintf("copy src path and dst path can not be the same, src: %s dst: %s", binlog.GetLogPath(), targetPath))
				}

				binlog := binlog
				job := func(ctx context.Context) error {
					exist, err := b.getStorageClient().Exist(ctx, b.milvusBucketName, binlog.GetLogPath())
					if err != nil {
						log.Info("Fail to check file exist",
							zap.Error(err),
							zap.String("file", binlog.GetLogPath()))
						return err
					}
					if !exist {
						log.Error("Binlog file not exist",
							zap.Error(err),
							zap.String("file", binlog.GetLogPath()))
						return err
					}

					err = b.getStorageClient().Copy(ctx, b.milvusBucketName, b.backupBucketName, binlog.GetLogPath(), targetPath)
					if err != nil {
						log.Info("Fail to copy file",
							zap.Error(err),
							zap.String("from", binlog.GetLogPath()),
							zap.String("to", targetPath))
						return err
					} else {
						log.Debug("Successfully copy file",
							zap.String("from", binlog.GetLogPath()),
							zap.String("to", targetPath))
					}

					return nil
				}
				wp.Submit(job)
			}
		}
		// delta log
		for _, binlogs := range segment.GetDeltalogs() {
			for _, binlog := range binlogs.GetBinlogs() {
				targetPath := backupPathFunc(binlog.GetLogPath(), b.milvusRootPath, dstPath)
				if segment.GetGroupId() != 0 {
					targetPath = strings.Replace(targetPath,
						strconv.FormatInt(segment.GetPartitionId(), 10),
						strconv.FormatInt(segment.GetPartitionId(), 10)+"/"+strconv.FormatInt(segment.GetGroupId(), 10),
						1)
				}
				if targetPath == binlog.GetLogPath() {
					return errors.New(fmt.Sprintf("copy src path and dst path can not be the same, src: %s dst: %s", binlog.GetLogPath(), targetPath))
				}

				binlog := binlog
				job := func(ctx context.Context) error {
					exist, err := b.getStorageClient().Exist(ctx, b.milvusBucketName, binlog.GetLogPath())
					if err != nil {
						log.Info("Fail to check file exist",
							zap.Error(err),
							zap.String("file", binlog.GetLogPath()))
						return err
					}
					if !exist {
						log.Error("Binlog file not exist",
							zap.Error(err),
							zap.String("file", binlog.GetLogPath()))
						return errors.New("Binlog file not exist " + binlog.GetLogPath())
					}
					err = b.getStorageClient().Copy(ctx, b.milvusBucketName, b.backupBucketName, binlog.GetLogPath(), targetPath)
					if err != nil {
						log.Info("Fail to copy file",
							zap.Error(err),
							zap.String("from", binlog.GetLogPath()),
							zap.String("to", targetPath))
						return err
					} else {
						log.Debug("Successfully copy file",
							zap.String("from", binlog.GetLogPath()),
							zap.String("to", targetPath))
					}
					return err
				}
				wp.Submit(job)
			}
		}
		duration := time.Now().Unix() - start
		log.Debug("copy segment finished",
			zap.Int64("collection_id", segment.GetCollectionId()),
			zap.Int64("partition_id", segment.GetPartitionId()),
			zap.Int64("segment_id", segment.GetSegmentId()),
			zap.Int64("cost_time", duration))
	}
	wp.Done()
	if err := wp.Wait(); err != nil {
		return err
	}
	return nil
}

func (b *BackupContext) readSegmentInfo(ctx context.Context, collectionID int64, partitionID int64, segmentID int64, numOfRows int64) (*backuppb.SegmentBackupInfo, error) {
	segmentBackupInfo := backuppb.SegmentBackupInfo{
		SegmentId:    segmentID,
		CollectionId: collectionID,
		PartitionId:  partitionID,
		NumOfRows:    numOfRows,
	}
	var size int64 = 0
	var rootPath string

	if b.params.MinioCfg.RootPath != "" {
		rootPath = fmt.Sprintf("%s/", b.params.MinioCfg.RootPath)
	} else {
		rootPath = ""
	}

	insertPath := fmt.Sprintf("%s%s/%v/%v/%v/", rootPath, "insert_log", collectionID, partitionID, segmentID)
	log.Debug("insertPath", zap.String("bucket", b.milvusBucketName), zap.String("insertPath", insertPath))
	fieldsLogDir, _, err := b.getStorageClient().ListWithPrefix(ctx, b.milvusBucketName, insertPath, false)
	if err != nil {
		log.Error("Fail to list segment path", zap.String("insertPath", insertPath), zap.Error(err))
		return &segmentBackupInfo, err
	}
	log.Debug("fieldsLogDir", zap.String("bucket", b.milvusBucketName), zap.Any("fieldsLogDir", fieldsLogDir))
	insertLogs := make([]*backuppb.FieldBinlog, 0)
	for _, fieldLogDir := range fieldsLogDir {
		binlogPaths, sizes, _ := b.getStorageClient().ListWithPrefix(ctx, b.milvusBucketName, fieldLogDir, false)
		fieldIdStr := strings.Replace(strings.Replace(fieldLogDir, insertPath, "", 1), SEPERATOR, "", -1)
		fieldId, _ := strconv.ParseInt(fieldIdStr, 10, 64)
		binlogs := make([]*backuppb.Binlog, 0)
		for index, binlogPath := range binlogPaths {
			binlogs = append(binlogs, &backuppb.Binlog{
				LogPath: binlogPath,
				LogSize: sizes[index],
			})
			size += sizes[index]
		}
		insertLogs = append(insertLogs, &backuppb.FieldBinlog{
			FieldID: fieldId,
			Binlogs: binlogs,
		})
	}

	deltaLogPath := fmt.Sprintf("%s%s/%v/%v/%v/", rootPath, "delta_log", collectionID, partitionID, segmentID)
	deltaFieldsLogDir, _, _ := b.getStorageClient().ListWithPrefix(ctx, b.milvusBucketName, deltaLogPath, false)
	deltaLogs := make([]*backuppb.FieldBinlog, 0)
	for _, deltaFieldLogDir := range deltaFieldsLogDir {
		binlogPaths, sizes, _ := b.getStorageClient().ListWithPrefix(ctx, b.milvusBucketName, deltaFieldLogDir, false)
		fieldIdStr := strings.Replace(strings.Replace(deltaFieldLogDir, deltaLogPath, "", 1), SEPERATOR, "", -1)
		fieldId, _ := strconv.ParseInt(fieldIdStr, 10, 64)
		binlogs := make([]*backuppb.Binlog, 0)
		for index, binlogPath := range binlogPaths {
			binlogs = append(binlogs, &backuppb.Binlog{
				LogPath: binlogPath,
				LogSize: sizes[index],
			})
			size += sizes[index]
		}
		deltaLogs = append(deltaLogs, &backuppb.FieldBinlog{
			FieldID: fieldId,
			Binlogs: binlogs,
		})
	}
	if len(deltaLogs) == 0 {
		deltaLogs = append(deltaLogs, &backuppb.FieldBinlog{
			FieldID: 0,
		})
	}

	//statsLogPath := fmt.Sprintf("%s/%s/%v/%v/%v/", b.params.MinioCfg.RootPath, "stats_log", collectionID, partitionID, segmentID)
	//statsFieldsLogDir, _, _ := b.storageClient.ListWithPrefix(ctx, b.milvusBucketName, statsLogPath, false)
	//statsLogs := make([]*backuppb.FieldBinlog, 0)
	//for _, statsFieldLogDir := range statsFieldsLogDir {
	//	binlogPaths, sizes, _ := b.storageClient.ListWithPrefix(ctx, b.milvusBucketName, statsFieldLogDir, false)
	//	fieldIdStr := strings.Replace(strings.Replace(statsFieldLogDir, statsLogPath, "", 1), SEPERATOR, "", -1)
	//	fieldId, _ := strconv.ParseInt(fieldIdStr, 10, 64)
	//	binlogs := make([]*backuppb.Binlog, 0)
	//	for index, binlogPath := range binlogPaths {
	//		binlogs = append(binlogs, &backuppb.Binlog{
	//			LogPath: binlogPath,
	//			LogSize: sizes[index],
	//		})
	//	}
	//	statsLogs = append(statsLogs, &backuppb.FieldBinlog{
	//		FieldID: fieldId,
	//		Binlogs: binlogs,
	//	})
	//}

	segmentBackupInfo.Binlogs = insertLogs
	segmentBackupInfo.Deltalogs = deltaLogs
	//segmentBackupInfo.Statslogs = statsLogs

	segmentBackupInfo.Size = size
	return &segmentBackupInfo, nil
}
