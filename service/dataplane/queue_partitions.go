package dataplane

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ShufflePartition maps a conversation onto one of the tenant's shuffle-shard
// partitions: each tenant deterministically owns shardsPerTenant of the
// partitions pool, and a conversation always lands on the same one so its
// turns stay ordered.
func ShufflePartition(tenantID int64, conversationID string, partitions, shardsPerTenant int) (int, error) {
	if partitions <= 0 || shardsPerTenant <= 0 || shardsPerTenant > partitions {
		return 0, fmt.Errorf("%w: partitions and shards per tenant must be positive with shards <= partitions", domain.ErrValidation)
	}
	shards := tenantShards(tenantID, partitions, shardsPerTenant)
	return shards[int(hash64(conversationID)%uint64(len(shards)))], nil
}

func tenantShards(tenantID int64, partitions, shardsPerTenant int) []int {
	seed := hash64(fmt.Sprintf("tenant:%d", tenantID))
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	shards := random.Perm(partitions)[:shardsPerTenant]
	sort.Ints(shards)
	return shards
}

func hash64(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return hash.Sum64()
}

// tenantCandidate is one tenant with ready work, as seen by the fair picker.
type tenantCandidate struct {
	tenantID int64
	leased   int
	bestRank int
	oldest   int64
}

// pickTenants orders candidates by fairness: fewest leased turns per weight
// first, then priority rank, then oldest ready message; tenants at their
// concurrency ceiling are dropped. The caller claims in this order and moves on
// when a CAS claim loses a race.
func pickTenants(ctx context.Context, policy contract.TenantWeightPolicy, candidates []tenantCandidate) ([]tenantCandidate, error) {
	type scored struct {
		tenantCandidate
		score float64
	}
	ordered := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		weight, maxConcurrent := 1, 0
		if policy != nil {
			var err error
			if weight, maxConcurrent, err = policy.SchedulingWeight(ctx, candidate.tenantID); err != nil {
				return nil, fmt.Errorf("scheduling weight for tenant %d: %w", candidate.tenantID, err)
			}
			if weight < 1 {
				return nil, fmt.Errorf("%w: scheduling weight for tenant %d must be >= 1", domain.ErrValidation, candidate.tenantID)
			}
		}
		if maxConcurrent > 0 && candidate.leased >= maxConcurrent {
			continue
		}
		ordered = append(ordered, scored{candidate, float64(candidate.leased) / float64(weight)})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score < ordered[j].score
		}
		if ordered[i].bestRank != ordered[j].bestRank {
			return ordered[i].bestRank < ordered[j].bestRank
		}
		if ordered[i].oldest != ordered[j].oldest {
			return ordered[i].oldest < ordered[j].oldest
		}
		return ordered[i].tenantID < ordered[j].tenantID
	})
	result := make([]tenantCandidate, len(ordered))
	for i, candidate := range ordered {
		result[i] = candidate.tenantCandidate
	}
	return result, nil
}
