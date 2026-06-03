## analytics.sessions

| column | type | null | default | notes |
|---|---|---|---|---|
| id | bigint | NO |  | PK |
| user_id | bigint | NO |  | FK → users.id |
| started_at | timestamptz | NO | now() |  |

## public.order_items

| column | type | null | default | notes |
|---|---|---|---|---|
| order_id | bigint | NO |  | PK; FK → orders.id |
| line_no | integer | NO |  | PK |
| sku | text | NO |  |  |

## public.orders

| column | type | null | default | notes |
|---|---|---|---|---|
| id | bigint | NO | nextval('orders_id_seq'::regclass) | PK |
| user_id | bigint | NO |  | FK → users.id |
| status | order_status | NO | 'pending'::order_status |  |
| total | numeric | NO | 0 | Order total in USD. |
| placed_at | timestamptz | NO | now() |  |

## public.users

Customer accounts.

| column | type | null | default | notes |
|---|---|---|---|---|
| id | bigint | NO | nextval('users_id_seq'::regclass) | PK |
| email | text | NO |  |  |
| name | text | YES |  |  |
| created_at | timestamptz | NO | now() |  |
