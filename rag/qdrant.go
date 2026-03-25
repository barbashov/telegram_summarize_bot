package rag

import (
	"context"
	"fmt"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type qdrantStore struct {
	conn       *grpc.ClientConn
	points     pb.PointsClient
	collection pb.CollectionsClient
	collName   string
}

func newQdrantStore(addr, collName string) (*qdrantStore, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("qdrant dial: %w", err)
	}

	return &qdrantStore{
		conn:       conn,
		points:     pb.NewPointsClient(conn),
		collection: pb.NewCollectionsClient(conn),
		collName:   collName,
	}, nil
}

func (q *qdrantStore) close() error {
	return q.conn.Close()
}

func (q *qdrantStore) ensureCollection(ctx context.Context, dims uint64) error {
	resp, err := q.collection.List(ctx, &pb.ListCollectionsRequest{})
	if err != nil {
		return fmt.Errorf("qdrant list collections: %w", err)
	}

	for _, c := range resp.GetCollections() {
		if c.GetName() == q.collName {
			return q.ensureIndexes(ctx)
		}
	}

	distance := pb.Distance_Cosine
	_, err = q.collection.Create(ctx, &pb.CreateCollection{
		CollectionName: q.collName,
		VectorsConfig: &pb.VectorsConfig{
			Config: &pb.VectorsConfig_Params{
				Params: &pb.VectorParams{
					Size:     dims,
					Distance: distance,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("qdrant create collection: %w", err)
	}

	return q.ensureIndexes(ctx)
}

func (q *qdrantStore) ensureIndexes(ctx context.Context) error {
	indexes := []struct {
		field string
		typ   pb.FieldType
	}{
		{"group_id", pb.FieldType_FieldTypeKeyword},
		{"timestamp", pb.FieldType_FieldTypeInteger},
		{"tg_message_id", pb.FieldType_FieldTypeKeyword},
	}

	for _, idx := range indexes {
		_, err := q.points.CreateFieldIndex(ctx, &pb.CreateFieldIndexCollection{
			CollectionName: q.collName,
			FieldName:      idx.field,
			FieldType:      &idx.typ,
			Wait:           boolPtr(true),
		})
		if err != nil {
			// Index may already exist — Qdrant returns an error but it's safe to ignore.
			_ = err
		}
	}
	return nil
}

type vectorPoint struct {
	ID            string
	Vector        []float32
	GroupID       string
	Timestamp     int64
	TgMessageID   string
	Text          string
	UserHash      string
	ForwardedFrom string
	ReplyToTgID   string
}

func (q *qdrantStore) upsert(ctx context.Context, points []vectorPoint) error {
	if len(points) == 0 {
		return nil
	}

	pbPoints := make([]*pb.PointStruct, len(points))
	for i, p := range points {
		payload := map[string]*pb.Value{
			"group_id":      {Kind: &pb.Value_StringValue{StringValue: p.GroupID}},
			"timestamp":     {Kind: &pb.Value_IntegerValue{IntegerValue: p.Timestamp}},
			"tg_message_id": {Kind: &pb.Value_StringValue{StringValue: p.TgMessageID}},
			"text":          {Kind: &pb.Value_StringValue{StringValue: p.Text}},
			"user_hash":     {Kind: &pb.Value_StringValue{StringValue: p.UserHash}},
		}
		if p.ForwardedFrom != "" {
			payload["forwarded_from"] = &pb.Value{Kind: &pb.Value_StringValue{StringValue: p.ForwardedFrom}}
		}
		if p.ReplyToTgID != "" {
			payload["reply_to_tg_id"] = &pb.Value{Kind: &pb.Value_StringValue{StringValue: p.ReplyToTgID}}
		}

		pbPoints[i] = &pb.PointStruct{
			Id:      &pb.PointId{PointIdOptions: &pb.PointId_Uuid{Uuid: p.ID}},
			Vectors: &pb.Vectors{VectorsOptions: &pb.Vectors_Vector{Vector: &pb.Vector{Data: p.Vector}}},
			Payload: payload,
		}
	}

	_, err := q.points.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: q.collName,
		Points:         pbPoints,
		Wait:           boolPtr(true),
	})
	return err
}

type searchResult struct {
	Score       float32
	Text        string
	UserHash    string
	Timestamp   int64
	TgMessageID string
	GroupID     string
}

func (q *qdrantStore) search(ctx context.Context, vector []float32, groupID string, topK uint64) ([]searchResult, error) {
	resp, err := q.points.Search(ctx, &pb.SearchPoints{
		CollectionName: q.collName,
		Vector:         vector,
		Limit:          topK,
		Filter: &pb.Filter{
			Must: []*pb.Condition{
				{
					ConditionOneOf: &pb.Condition_Field{
						Field: &pb.FieldCondition{
							Key: "group_id",
							Match: &pb.Match{
								MatchValue: &pb.Match_Keyword{Keyword: groupID},
							},
						},
					},
				},
			},
		},
		WithPayload: &pb.WithPayloadSelector{
			SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant search: %w", err)
	}

	results := make([]searchResult, 0, len(resp.GetResult()))
	for _, hit := range resp.GetResult() {
		r := searchResult{Score: hit.GetScore()}
		for k, v := range hit.GetPayload() {
			switch k {
			case "text":
				r.Text = v.GetStringValue()
			case "user_hash":
				r.UserHash = v.GetStringValue()
			case "timestamp":
				r.Timestamp = v.GetIntegerValue()
			case "tg_message_id":
				r.TgMessageID = v.GetStringValue()
			case "group_id":
				r.GroupID = v.GetStringValue()
			}
		}
		results = append(results, r)
	}
	return results, nil
}

func (q *qdrantStore) searchByTimeRange(ctx context.Context, groupID string, minTS, maxTS int64) ([]searchResult, error) {
	resp, err := q.points.Scroll(ctx, &pb.ScrollPoints{
		CollectionName: q.collName,
		Filter: &pb.Filter{
			Must: []*pb.Condition{
				{
					ConditionOneOf: &pb.Condition_Field{
						Field: &pb.FieldCondition{
							Key: "group_id",
							Match: &pb.Match{
								MatchValue: &pb.Match_Keyword{Keyword: groupID},
							},
						},
					},
				},
				{
					ConditionOneOf: &pb.Condition_Field{
						Field: &pb.FieldCondition{
							Key: "timestamp",
							Range: &pb.Range{
								Gte: float64Ptr(float64(minTS)),
								Lte: float64Ptr(float64(maxTS)),
							},
						},
					},
				},
			},
		},
		WithPayload: &pb.WithPayloadSelector{
			SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true},
		},
		Limit: uint32Ptr(200),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant scroll: %w", err)
	}

	results := make([]searchResult, 0, len(resp.GetResult()))
	for _, pt := range resp.GetResult() {
		r := searchResult{}
		for k, v := range pt.GetPayload() {
			switch k {
			case "text":
				r.Text = v.GetStringValue()
			case "user_hash":
				r.UserHash = v.GetStringValue()
			case "timestamp":
				r.Timestamp = v.GetIntegerValue()
			case "tg_message_id":
				r.TgMessageID = v.GetStringValue()
			case "group_id":
				r.GroupID = v.GetStringValue()
			}
		}
		results = append(results, r)
	}
	return results, nil
}

func (q *qdrantStore) deleteByGroup(ctx context.Context, groupID string) error {
	_, err := q.points.Delete(ctx, &pb.DeletePoints{
		CollectionName: q.collName,
		Points: &pb.PointsSelector{
			PointsSelectorOneOf: &pb.PointsSelector_Filter{
				Filter: &pb.Filter{
					Must: []*pb.Condition{
						{
							ConditionOneOf: &pb.Condition_Field{
								Field: &pb.FieldCondition{
									Key: "group_id",
									Match: &pb.Match{
										MatchValue: &pb.Match_Keyword{Keyword: groupID},
									},
								},
							},
						},
					},
				},
			},
		},
		Wait: boolPtr(true),
	})
	return err
}

func boolPtr(v bool) *bool          { return &v }
func float64Ptr(v float64) *float64 { return &v }
func uint32Ptr(v uint32) *uint32    { return &v }
