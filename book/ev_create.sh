#!/bin/bash
curl -v --cookie session_id=7c6d03ef-aba0-4a43-b284-3f15e598b020 -X POST http://arch.homework/events/create -d '{"event_name":"green run", "price": 500, "total_slots":3}'
