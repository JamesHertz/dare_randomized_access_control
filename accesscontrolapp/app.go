package accesscontrolapp

import (
	"crypto/sha256"
	_ "crypto/sha256"
	"dare_randomized_access_control/cointoss"
	"encoding/binary"
	"fmt"
	"github.com/cloudflare/circl/group"
	"github.com/cloudflare/circl/secretsharing"
	"github.com/google/uuid"
	"github.com/negrel/assert"
	"github.com/petar/GoLLRB/llrb"
	"github.com/samber/lo"
	"log/slog"
	"math/rand"
	"unsafe"
)

type Msg struct {
	Issuer  uuid.UUID
	Content string
}

type User struct {
	Id     uuid.UUID
	Points *llrb.LLRB
}

type pt struct {
	pt int
}

func (p *pt) Less(than llrb.Item) bool {
	return p.pt < than.(*pt).pt
}

type App struct {
	secret secretsharing.Share
	users  map[uuid.UUID]*User
	msgs   []Msg
}

func ExecuteCRDT(crdt *CRDT) (*App, error) {
	app := NewApp()
	opList := crdt.GetOperationList()
	i := 0
	for i < len(opList) {
		op := opList[i]
		switch op.kind {
		case Init:
			err := execInit(op, app)
			if err != nil {
				return app, err
			}
			i++
		case Add:
			err := execAdd(op, app)
			if err != nil {
				slog.Warn("Unable to compute add operation", "err", err, "idx", op.idx, "op", op.content.(*AddOp))
			}
			i++
		case Rem:
			if isConcurrent(opList, i) {
				err := execConcurrent(op, opList[i+1], op.idx, app)
				if err != nil {
					slog.Warn("Unable to compute concurrent removal operation", "err", err, "idx", op.idx, "op", op.content.(*RemOp))
					i++
				} else {
					i += 2
				}
			} else {
				err := execRem(op, app)
				if err != nil {
					slog.Warn("Unable to compute removal operation", "err", err, "idx", op.idx, "op", op.content.(*RemOp))
				}
				i++
			}
		case Post:
			err := execPost(op, app)
			if err != nil {
				slog.Warn("Unable to compute post operation", "err", err, "idx", op.idx, "op", op.content.(*PostOp))
			}
			i++
		default:
			return app, fmt.Errorf("unhandled operation type")
		}
	}
	return app, nil
}

func isConcurrent(opList []*Op, i int) bool {
	assert.Equal(Rem, opList[i].kind, "First operation must be removal when this method is called")
	if (i+1) >= len(opList) || opList[i+1].kind != Rem {
		return false
	}
	rem1 := opList[i].content.(*RemOp)
	rem2 := opList[i+1].content.(*RemOp)
	return rem1.issuer == rem2.removed && rem1.removed == rem2.issuer
}

func execPost(op *Op, app *App) error {
	postOp := op.content.(*PostOp)
	err := app.Post(postOp, op.prevIds)
	if err != nil {
		return fmt.Errorf("unable to compute post operation: %v", err)
	}
	return nil
}

func execConcurrent(op1, op2 *Op, seed int64, app *App) error {
	rem1 := op1.content.(*RemOp)
	rem2 := op2.content.(*RemOp)
	canRem, reason := app.canRemUser(rem1)
	if !canRem {
		return fmt.Errorf(reason)
	}
	coin, err := computeCoinToss(seed, app.secret)
	if err != nil {
		return fmt.Errorf("unable to compute coin toss: %v", err)
	}
	threshold := app.computeThreshold(rem1)
	if coin < threshold {
		return app.RemUser(rem1, op1.prevIds)
	} else {
		return app.RemUser(rem2, op2.prevIds)
	}
}

func execRem(op *Op, app *App) error {
	remOp := op.content.(*RemOp)
	err := app.RemUser(remOp, op.prevIds)
	if err != nil {
		return fmt.Errorf("unable to compute remove operation: %v", err)
	}
	return nil
}

func execInit(op *Op, app *App) error {
	initOp := op.content.(*InitOp)
	err := app.Init(initOp)
	if err != nil {
		return fmt.Errorf("unable to compute init operation: %v", err)
	}
	return nil
}

func execAdd(op *Op, app *App) error {
	addOp := op.content.(*AddOp)
	err := app.AddUser(addOp, op.prevIds)
	if err != nil {
		return fmt.Errorf("unable to compute add operation: %v", err)
	}
	return nil
}

func NewApp() *App {
	r := rand.New(rand.NewSource(int64(0)))
	share := secretsharing.Share{
		ID:    group.Ristretto255.NewScalar(),
		Value: group.Ristretto255.RandomScalar(r),
	}
	return &App{
		secret: share,
		users:  make(map[uuid.UUID]*User),
		msgs:   make([]Msg, 0),
	}
}

func (app *App) Init(op *InitOp) error {
	if len(app.users) != 0 {
		return fmt.Errorf("already initialized")
	}
	pts := llrb.New()
	for _, p := range lo.Range(int(op.points)) {
		pts.InsertNoReplace(&pt{pt: p})
	}
	points := lo.Map(lo.Range(int(op.points)), func(p int, _ int) uint { return uint(p) })
	user := newUser(op.initial, points)
	app.users[op.initial] = user
	return nil
}

func (app *App) AddUser(op *AddOp, prevIds []uuid.UUID) error {
	if op.issuer == op.added {
		return fmt.Errorf("user cannot add themselves")
	} else if len(op.points) == 0 {
		return fmt.Errorf("at least a single point must be given")
	}
	issuer := app.users[op.issuer]
	if issuer == nil {
		return fmt.Errorf("operation issuer is not a user")
	} else if len(op.points) >= issuer.Points.Len() {
		return fmt.Errorf("issuer cannot give more or equal points than what they have")
	} else if !lo.EveryBy(op.points, func(p uint) bool { return issuer.Points.Has(&pt{pt: int(p)}) }) {
		return fmt.Errorf("issuer cannot give points they do not have")
	}
	if app.users[op.added] != nil {
		return fmt.Errorf("added user already exists")
	}
	for _, p := range op.points {
		issuer.Points.Delete(&pt{pt: int(p)})
	}
	added := newUser(op.added, op.points)
	app.users[op.added] = added
	return nil
}

func (app *App) computeThreshold(rem1 *RemOp) float64 {
	issuer := app.users[rem1.issuer]
	removed := app.users[rem1.removed]
	issuerPoints := issuer.Points.Len()
	totalPoints := issuerPoints + removed.Points.Len()
	threshold := float64(issuerPoints) / float64(totalPoints)
	return threshold
}

func computeCoinToss(seed int64, secret secretsharing.Share) (float64, error) {
	seedBytes := make([]byte, unsafe.Sizeof(seed))
	binary.LittleEndian.PutUint64(seedBytes, uint64(seed))
	hash := sha256.Sum256(seedBytes)
	base := group.Ristretto255.HashToElement(hash[:], []byte("concurrent_rem_base"))
	point := cointoss.ShareToPoint(secret, base)
	coin, err := cointoss.HashPointToDouble(point.Point)
	if err != nil {
		return 0, fmt.Errorf("unable to hash secret point to number: %v", err)
	}
	return coin, nil
}

func (app *App) RemUser(op *RemOp, prevIds []uuid.UUID) error {
	canRem, reason := app.canRemUser(op)
	if !canRem {
		return fmt.Errorf(reason)
	}
	issuer := app.users[op.issuer]
	removed := app.users[op.removed]
	assert.True(areSetsDisjoint(issuer.Points, removed.Points), "points must be disjoint")
	transferPoints(removed.Points, issuer.Points)
	delete(app.users, op.removed)
	return nil
}

func transferPoints(from, to *llrb.LLRB) {
	from.AscendGreaterOrEqual(from.Min(), func(val llrb.Item) bool {
		to.InsertNoReplace(val)
		return true
	})
}

func areSetsDisjoint(a, b *llrb.LLRB) bool {
	disjoint := true
	a.AscendGreaterOrEqual(a.Min(), func(val llrb.Item) bool {
		if b.Has(val) {
			disjoint = false
			return false
		}
		return true
	})
	return disjoint
}

func (app *App) canRemUser(op *RemOp) (bool, string) {
	if op.issuer == op.removed {
		return false, "user cannot remove themselves"
	}
	issuer := app.users[op.issuer]
	removed := app.users[op.removed]
	if issuer == nil {
		return false, "operation issuer is not a user"
	} else if removed == nil {
		return false, "removed user is not in the system"
	}
	return true, ""
}

func (app *App) Post(op *PostOp, prevIds []uuid.UUID) error {
	poster := app.users[op.poster]
	if poster == nil {
		return fmt.Errorf("operation poster is not a user")
	}
	msg := Msg{
		Issuer:  op.poster,
		Content: op.msg,
	}
	app.msgs = append(app.msgs, msg)
	return nil
}

func newUser(id uuid.UUID, points []uint) *User {
	pts := llrb.New()
	for _, p := range points {
		pts.InsertNoReplace(&pt{pt: int(p)})
	}
	return &User{
		Id:     id,
		Points: pts,
	}
}
