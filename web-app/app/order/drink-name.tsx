import { orderParts } from "./drinks";

// DrinkName shows an order as "Lungo, decaf": the drink in the surrounding
// style, the beans after it in a lighter tone, so they read as a qualifier on
// the drink rather than part of its name. Regular beans show the drink alone.
export function DrinkName({
  drink,
  beans,
  beansClassName = "text-neutral-400",
}: {
  drink: string;
  beans?: string;
  beansClassName?: string;
}) {
  const p = orderParts(drink, beans);
  return (
    <>
      {p.drink}
      {p.drink && p.beans && (
        <span className={beansClassName}>, {p.beans}</span>
      )}
    </>
  );
}
