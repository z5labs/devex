public.orders: CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)
public.users: CREATE UNIQUE INDEX users_email_key ON public.users USING btree (email)
public.users: CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)
