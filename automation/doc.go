// Package automation runs background processors that work off a todo list.
//
// In Event Modeling, an automation is a processor that looks at a read model
// listing pending work (the todo list), does that work, and records the
// outcome by sending a command. The resulting event removes the item from
// the todo list, which closes the loop:
//
//	OrderPlaced ──► orders to ship (todo list) ──► ship-orders automation
//	     ▲                                                │
//	     └──────── OrderShipped ◄──── ShipOrder ◄─────────┘
//
// The todo list is an ordinary projection (see the projection package),
// usually a table. The automation never reacts to events directly; it asks
// the todo list what is pending. That keeps the automation stateless,
// makes stuck work visible with a query, and lets a crashed automation
// resume by simply asking again.
//
// # Writing an automation
//
// An automation needs two functions next to the todo list's projection: one
// that claims pending items, and one that does the work for one item.
//
//	// Claim reads pending items from the todo list and leases them, so that
//	// other instances skip them for a while.
//	func (p *OrdersToShip) Claim(ctx context.Context, max int, lease time.Duration) ([]ShippingTodo, error) {
//		rows, err := p.pool.Query(ctx, `
//			UPDATE orders_to_ship SET claimed_until = now() + $2
//			WHERE order_id IN (
//				SELECT order_id FROM orders_to_ship
//				WHERE claimed_until IS NULL OR claimed_until < now()
//				ORDER BY order_id
//				LIMIT $1
//				FOR UPDATE SKIP LOCKED)
//			RETURNING order_id, address`, max, lease)
//		if err != nil {
//			return nil, err
//		}
//		return pgx.CollectRows(rows, pgx.RowToStructByPos[ShippingTodo])
//	}
//
//	// ShipOrder does the work for one item and records the outcome.
//	func ShipOrder(bus eventsourcing.Dispatcher, carrier Carrier) automation.WorkFunc[ShippingTodo] {
//		return func(ctx context.Context, t ShippingTodo) error {
//			tracking, err := carrier.CreateShipment(ctx, t.OrderID, t.Address)
//			if err != nil {
//				return err
//			}
//			_, err = bus.Dispatch(ctx, ShipOrder{OrderID: t.OrderID, TrackingNo: tracking})
//			return err
//		}
//	}
//
// Wiring the todo list and the automation together:
//
//	ordersToShip := shipping.NewOrdersToShip(pool)
//	todoList := projection.NewRunner("orders-to-ship", ordersToShip.EventHandlers(), store,
//		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
//	)
//
//	ship := automation.New("ship-orders", todoList,
//		ordersToShip.Claim,
//		shipping.ShipOrder(commandBus, carrier),
//	)
//
//	go todoList.Run(ctx)
//	go ship.Run(ctx)
//
// # Waiting for the todo list
//
// An automation only claims work while its todo list is live. While the todo
// list is being rebuilt, it temporarily lists items that are already done
// further along in the log; an automation acting on them would repeat work
// that was finished long ago. The same goes for a todo list that is stalled
// on a bad event, or whose instance died: completed items would never
// disappear from it.
//
// The automation therefore checks its todo list, and anything passed with
// [WaitFor], before every claim, not only at start. Items claimed before a
// dependency stopped being live are finished normally.
//
// The todo list may run in another process or another program; pass a
// [projection.Remote] dependency instead of a runner. See
// [projection.Dependency].
//
// # Doing the work safely
//
// Work is delivered at least once. An item whose work failed, or whose
// automation crashed halfway, is claimed again once its lease expires. The
// work must therefore be safe to repeat:
//
//   - The command it sends should be idempotent: a command handler that
//     returns no events for work already recorded (an order already
//     shipped) makes a repeated command harmless.
//   - Calls to external systems should carry an idempotency key, typically
//     the item's ID, so that a retry after a crash does not create a
//     second shipment.
//
// The work function receives a ctx carrying the automation's name as
// causation (see eventsourcing.CausationFromContext), so events recorded
// by its commands can be traced back to it.
//
// # Waking up
//
// An automation claims work at every interval ([WithInterval]), and
// immediately when nudged. When its dependencies implement
// [projection.Watcher], as a [projection.Runner] does, the automation is
// nudged whenever the todo list commits new events or becomes live, so
// new work is picked up within milliseconds.
package automation
