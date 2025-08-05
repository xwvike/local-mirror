<script lang="ts">
	import * as AlertDialog from '$lib/components/ui/alert-dialog/index.js';
	import { buttonVariants } from '$lib/components/ui/button/index.js';
</script>

<AlertDialog.Root>
	<AlertDialog.Trigger class={buttonVariants({ variant: 'outline' })}>
		Show Dialog
	</AlertDialog.Trigger>
	<AlertDialog.Content class="" portalProps={{}}>
		<AlertDialog.Header class="">
			<AlertDialog.Title class="">Are you absolutely sure?</AlertDialog.Title>
			<AlertDialog.Description class="">
				This action cannot be undone. This will permanently delete your account and remove your data
				from our servers.
			</AlertDialog.Description>
		</AlertDialog.Header>
		<AlertDialog.Footer class="">
			<AlertDialog.Cancel class="">Cancel</AlertDialog.Cancel>
			<AlertDialog.Action class="">Continue</AlertDialog.Action>
		</AlertDialog.Footer>
	</AlertDialog.Content>
</AlertDialog.Root>
<h1>Welcome to SvelteKit</h1>
<p>
	Visit <a class="text-xl" href="https://svelte.dev/docs/kit">svelte.dev/docs/kit</a> to read the documentation
</p>
