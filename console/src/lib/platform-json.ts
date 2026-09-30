/** Read a protobuf JSON int64 only where the UI needs a JavaScript number. */
export function safeInteger(value: string): number {
	if (!/^-?\d+$/.test(value))
		throw new Error(`Invalid protobuf integer: ${value}`);
	const result = Number(value);
	if (!Number.isSafeInteger(result))
		throw new Error(
			`Protobuf integer exceeds JavaScript's safe range: ${value}`,
		);
	return result;
}

/** Encode a validated UI integer without risking a rounded int64. */
export function integerString(value: number): string {
	if (!Number.isSafeInteger(value))
		throw new Error(`Invalid integer: ${value}`);
	return String(value);
}

import {
	type DescMessage,
	fromJsonString,
	type Message,
	type MessageJsonType,
	type MessageShape,
	toJson,
} from "@bufbuild/protobuf";

type RequiredJsonKeys<M, J> = {
	[K in keyof J]-?: K extends keyof M
		? undefined extends M[K]
			? never
			: K
		: never;
}[keyof J];
type SelectedOneof<T, K> = T extends { case: K; value: infer V } ? V : never;
type NativeField<M, K> = K extends keyof M
	? M[K]
	: { [P in keyof M]: SelectedOneof<M[P], K> }[keyof M];
type JsonField<M, J> = M extends Message
	? J extends object
		? DefaultedMessageJson<M, J>
		: J
	: M extends (infer E)[]
		? J extends (infer V)[]
			? JsonField<E, V>[]
			: J
		: M extends object
			? J extends object
				? { [K in keyof J]: K extends keyof M ? JsonField<M[K], J[K]> : J[K] }
				: J
			: J;
type DefaultedMessageJson<M, J> = {
	[K in RequiredJsonKeys<M, J>]-?: K extends keyof M
		? JsonField<NonNullable<M[K]>, NonNullable<J[K]>>
		: never;
} & {
	[K in Exclude<keyof J, RequiredJsonKeys<M, J>>]?: [
		NativeField<M, K>,
	] extends [never]
		? J[K]
		: JsonField<NonNullable<NativeField<M, K>>, NonNullable<J[K]>>;
};

/** JSON emitted with scalar/list/map defaults; message and oneof presence remains optional. */
export type PlatformJson<D extends DescMessage> = DefaultedMessageJson<
	MessageShape<D>,
	MessageJsonType<D>
>;

export function toPlatformJson<D extends DescMessage>(
	schema: D,
	message: MessageShape<D>,
): PlatformJson<D> {
	// The native generated message establishes presence; the canonical codec emits
	// its required scalar/list/map defaults. This assertion describes that guarantee.
	return toJson(schema, message, {
		alwaysEmitImplicit: true,
	}) as PlatformJson<D>;
}

export function compareIntegers(left: string, right: string): number {
	const a = BigInt(left);
	const b = BigInt(right);
	return a < b ? -1 : a > b ? 1 : 0;
}

export function fromPlatformJson<D extends DescMessage>(
	schema: D,
	value: MessageJsonType<D>,
): MessageShape<D> {
	return fromJsonString(schema, JSON.stringify(value));
}
