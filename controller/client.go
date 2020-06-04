package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
	"gitlab.com/ti-backend/go-modules/frame"
	"gitlab.com/ti-backend/go-modules/frame/code"
	"gitlab.com/ti-backend/go-modules/ulgsdk/order"
	"gitlab.com/ti-backend/go-modules/utils/timetool"
	"gitlab.com/ti-backend/ulg168/blackjack/conf"
	"gitlab.com/ti-backend/ulg168/blackjack/protocol"
	"gitlab.com/ti-backend/ulg168/blackjack/protocol/action"
	"gitlab.com/ti-backend/ulg168/blackjack/protocol/command"
)

type IOrder interface {
	GetBet() uint64
}

type client struct {
	betData     protocol.BetData
	betOrderRes map[string]IOrder
	ConnID      string
	Account     string
	Name        string
	Balance     float64
	Token       string
	GameToken   string
	BetAmount   map[string]float64
	GameRoom    string
	GameRound   string

	ctx  context.Context
	quit context.CancelFunc

	// in is the message queue which sent to WebSocket clients.
	in chan frame.Frame

	// out is a channel which the data sent to lobby.Player.
	out chan frame.Frame

	// conn holds the WebSocket client.
	conn *websocket.Conn
}

func (c *client) serve() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case f := <-c.in:
			if f.Status == code.PlayerNewerLogin {
				writeJSON(c.conn, NewS2CLoginRepeat())
				continue
			}

			switch f.Command {
			case command.Ask:
				if data, ok := f.Data.(protocol.Data); ok {
					amount := c.betOrderRes[fmt.Sprintf("%d-%s", data.No, action.Bet)].GetBet()
					if uint64(c.Balance) < amount {
						data.Options[action.Double] = false
						data.Options[action.Split] = false
					}

					if uint64(c.Balance) < amount/2 {
						data.Options[action.Insurance] = false
					}

					f.Data = data
				}

			case command.NewRound, command.TableResult:
				if t, ok := f.Data.(protocol.Table); ok {
					roundData := strings.Split(t.Round, "-")
					day := fmt.Sprintf("%s-%s-%s", roundData[0], roundData[1], roundData[2])
					round := fmt.Sprintf("%s-%s", roundData[3], roundData[4])
					r := fmt.Sprintf("%s-%03d-%s", day, t.ID, round)
					c.GameRound = r
					c.GameRoom = strconv.Itoa(t.ID)
				}

			case command.UpdateSeat:
				if seats, ok := f.Data.([]protocol.Seat); ok {
					for _, s := range seats {
						if s.Account == c.Account {
							// ...
						}
					}
				}

			case command.GameResult:
				if result, ok := f.Data.([]protocol.BetData); ok {
					basic := order.Basic{
						Token:     c.Token,
						GameToken: c.GameToken,
						GameID:    conf.GameID,
					}

					for _, d := range result {
						banker := strings.Join(d.Dealer.Codes(), ",")
						for k, v := range d.Action {
							key := k
							if key == action.Pay || key == action.GiveUp {
								key = action.Bet
							}
							o, found := c.betOrderRes[fmt.Sprintf("%d-%s", d.No, key)]
							if !found {
								continue
							}
							switch k {
							case action.Double:
								log.Printf("settlement: %s, %+v, %+v\n", k, v, o)
								v.Cards = d.Action[action.Bet].Cards
							case action.Insurance:
								log.Printf("settlement: %s, %+v, %+v\n", k, v, o)
								v.Cards = d.Dealer
							}
							settlement(basic, k, banker, v, c.betOrderRes)
						}
					}
					SendMemberInfo(c)

					var total float64
					for _, d := range result {
						for _, p := range d.Action {
							total += p.Pay - p.Bet
						}
					}

					f.Data = protocol.GameResult{
						ID:    result[0].ID,
						Round: result[0].Round,
						Win:   total,
					}
				}
			}

			if err := writeJSON(c.conn, f); err != nil {
				log.Println("controller:", err)
				if err == websocket.ErrCloseSent { // already closed
					c.Close()
				}
			}
		}
	}
}

func settlement(basic order.Basic, action, banker string, v protocol.Pile, o map[string]IOrder) {
	amount := v.Bet
	win := v.Pay
	res := strings.Join(v.Cards.Codes(), ",")
	now := timetool.GetNowByUTC()

	fmt.Println("amount", amount, "win", win, "res", res, "now", now, "o", o)
	// for _, oi := range o.OrderItems {
	// 	if oi.PlayCode != action {
	// 		continue
	// 	}
	// 	rate := win / amount
	// 	fmt.Println("settlement 1")
	// 	// TODO 子單結算
	// 	_, err := order.OrderItemPayout(o.UUID, oi.UUID, &order.PayoutOrderItem{
	// 		Basic:    basic,
	// 		Rate:     rate,
	// 		Win:      win,
	// 		Result:   res,
	// 		PayoutAt: now,
	// 	})
	// 	if err != nil {
	// 		log.Println("settlement error: ", err.Error())
	// 		continue
	// 	}

	// 	oi.Rate = &rate
	// 	oi.Win = &win
	// 	oi.Result = &res
	// 	oi.Status = 2
	// 	oi.PayoutAt = &now
	// }

	// payout := true
	// for _, oi := range o.OrderItems {
	// 	if oi.Status != 2 {
	// 		payout = false
	// 	}
	// }

	// if payout {
	// 	fmt.Println("settlement 2")
	// 	// TODO 母單結算
	// 	if _, err := order.OrderPayout(o.UUID, &order.PayoutOrder{
	// 		Basic:    basic,
	// 		Result:   banker,
	// 		Summary:  *o.Summary,
	// 		PayoutAt: now,
	// 	}); err != nil {
	// 		log.Println("settlement error: ", err.Error())
	// 	}
	// }
}

func (c *client) listenAndServe() {
	go c.serve()
	defer func() {
		// time.Sleep(5 * time.Second)
		close(c.out)
	}()

	for {
		req := Frame{Frame: frame.Frame{Data: &json.RawMessage{}}}

		if err := readJSON(c.conn, &req); err != nil {
			if isClosed(err) {
				return
			}
			c.write(NewS2CErrorAck(ECServerError, err))
			continue
		}

		if conf.LoginRepeatEnable && req.Command != CMDc2sLogin {
			// TODO 重複登入判斷 要移除的功能
			// res, err := operate.LoginValidate(&operate.AuthValidateCond{
			// 	Token:     c.Token,
			// 	GameToken: c.GameToken,
			// 	GameID:    conf.GameID,
			// 	ConnID:    c.ConnID,
			// })
			// if res.Status != 1 {
			// 	c.write(NewS2CLoginRepeat())
			// 	continue
			// }
		}

		ok, err := ActionCheck(c, &req)
		if err != nil {
			c.write(NewS2CErrorAck(ECServerError, err))
			continue
		}

		if ok {
			continue
		}

		d := protocol.Data{}
		if err = json.Unmarshal(*req.Data.(*json.RawMessage), &d); err != nil {
			continue
		}
		req.Data = d

		fmt.Println("client: ", req.Frame)
		c.out <- req.Frame
	}
}

func (c *client) Receive() <-chan frame.Frame {
	return c.out
}

func (c *client) Send() chan<- frame.Frame {
	return c.in
}

func (c *client) write(f frame.Frame) {
	select {
	case <-c.ctx.Done():
	case c.in <- f:
	}
}

func (c *client) Close() {
	c.quit()
}

func newClient(conn *websocket.Conn) *client {
	if conn == nil {
		panic("controller: nil websocket connection")
	}

	ctx, quit := context.WithCancel(context.Background())
	c := client{
		in:          make(chan frame.Frame, 8),
		out:         make(chan frame.Frame),
		ctx:         ctx,
		quit:        quit,
		conn:        conn,
		betOrderRes: make(map[string]IOrder),
		BetAmount:   make(map[string]float64),
	}
	go c.listenAndServe()

	return &c
}
