## public.active_users

Users active in the last 30 days.

```sql
SELECT u.id,
    u.email
   FROM users u
  WHERE (u.created_at > (now() - '30 days'::interval))
```
